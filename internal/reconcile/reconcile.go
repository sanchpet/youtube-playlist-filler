// Package reconcile brings a playlist into line with a set of channels.
//
// It is a reconciler, not an event handler: it keeps no cursor, no seen-set and no database. The
// playlist is the state, and every run rediscovers it from scratch. That is what makes a missed
// run, a crashed run and a run that inserted half its candidates all recover on their own — the
// next run simply sees a playlist that is missing fewer videos than it was.
//
// The order is fixed and the first step is not optional. playlistItems.insert is not idempotent:
// adding a video that is already there returns success and leaves the playlist holding it twice.
// Reading the playlist first is the only thing standing between a scheduled job and a playlist
// full of duplicates.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sanchpet/youtube-playlist-filler/internal/ytapi"
)

// Source yields the recent uploads of one channel: the public feed for the hourly run, the
// uploads playlist for the weekly full reconcile. It returns the number of billed API calls it
// made, which is zero for the feed.
type Source interface {
	VideoIDs(ctx context.Context, channelID string) (ids []string, calls int, err error)
}

// Playlist is the target playlist: read in full, then appended to.
type Playlist interface {
	PlaylistVideoIDs(ctx context.Context, playlistID string) (ids []string, calls int, err error)
	Durations(ctx context.Context, ids []string) (map[string]time.Duration, int, error)
	Insert(ctx context.Context, playlistID, videoID string) error
}

// Options is one run.
type Options struct {
	PlaylistID string
	Channels   []string

	// Min and Max are the closed duration band a video has to fall inside. Both bounds are load
	// bearing: the lower one is what keeps Shorts and trailers out, and an implementation that
	// only enforces the ceiling looks correct on every long video it rejects while admitting
	// everything short.
	Min, Max time.Duration

	// MaxInserts caps how many videos one run will add. It is a quota fuse, not a policy: at
	// fifty units an insert against a ten-thousand-unit day, an unbounded run that finds a
	// thousand candidates spends the entire budget in its first minutes and blinds every other
	// run until the reset.
	MaxInserts int

	DryRun bool
}

// Result is what a run did, for the summary line.
type Result struct {
	PlaylistSize int
	Candidates   int
	InBand       int
	Inserted     int
	// Deferred is candidates that passed the band but were not added — the insert cap was reached
	// or the playlist filled up. They are not lost: the next run rediscovers them.
	Deferred int
	Units    int
	DryRun   bool
	// PlaylistFull records that YouTube refused an insert because the playlist is at its limit.
	// A terminal condition for the run, and not an error: no amount of retrying or waiting
	// changes it, and a crash loop would only bury the one line that says so.
	PlaylistFull bool
}

// InBand reports whether a duration falls inside the closed band [min, max].
func InBand(d, minD, maxD time.Duration) bool {
	return d >= minD && d <= maxD
}

// Diff returns the candidates that are not already in the playlist, in the order given and with
// repeats removed.
//
// Keyed on the video id and nothing else. The same video carries different published and updated
// timestamps in the feed and in the API, so any key including them would report every video as new
// on every run.
func Diff(candidates []string, present map[string]struct{}) []string {
	out := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, id := range candidates {
		if _, ok := present[id]; ok {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// Run performs one reconciliation.
//
// A channel that cannot be read does not stop the others — its videos are simply not considered
// this time — but the run still ends in an error, so a feed that has been failing for a week is
// visible rather than merely quiet.
func Run(ctx context.Context, pl Playlist, src Source, opts Options, log *slog.Logger) (Result, error) {
	var res Result
	res.DryRun = opts.DryRun

	present, calls, err := pl.PlaylistVideoIDs(ctx, opts.PlaylistID)
	res.Units += calls * ytapi.CostList
	if err != nil {
		return res, fmt.Errorf("read playlist: %w", err)
	}
	res.PlaylistSize = len(present)
	inPlaylist := make(map[string]struct{}, len(present))
	for _, id := range present {
		inPlaylist[id] = struct{}{}
	}
	log.Info("playlist read", "playlist", opts.PlaylistID, "items", len(present), "distinct", len(inPlaylist))

	var (
		uploads    []string
		sourceErrs []error
	)
	for _, ch := range opts.Channels {
		ids, calls, err := src.VideoIDs(ctx, ch)
		res.Units += calls * ytapi.CostList
		if err != nil {
			log.Error("channel unreadable", "channel", ch, "err", err)
			sourceErrs = append(sourceErrs, err)
			continue
		}
		log.Debug("channel read", "channel", ch, "videos", len(ids))
		uploads = append(uploads, ids...)
	}

	candidates := Diff(uploads, inPlaylist)
	res.Candidates = len(candidates)
	log.Info("candidates", "uploads", len(uploads), "new", len(candidates))

	keep, calls, err := filterBand(ctx, pl, candidates, opts, log)
	res.Units += calls * ytapi.CostList
	if err != nil {
		return res, errors.Join(append(sourceErrs, err)...)
	}
	res.InBand = len(keep)

	if len(keep) > opts.MaxInserts {
		log.Warn("insert cap reached", "in_band", len(keep), "cap", opts.MaxInserts,
			"deferred", len(keep)-opts.MaxInserts)
		res.Deferred = len(keep) - opts.MaxInserts
		keep = keep[:opts.MaxInserts]
	}

	for _, id := range keep {
		if opts.DryRun {
			log.Info("would add", "video", id, "playlist", opts.PlaylistID)
			res.Inserted++
			continue
		}
		if err := pl.Insert(ctx, opts.PlaylistID, id); err != nil {
			res.Units += ytapi.CostInsert
			if errors.Is(err, ytapi.ErrPlaylistFull) {
				log.Error("playlist is full, stopping", "playlist", opts.PlaylistID,
					"inserted", res.Inserted, "remaining", len(keep)-res.Inserted, "err", err)
				res.PlaylistFull = true
				res.Deferred += len(keep) - res.Inserted
				return res, errors.Join(sourceErrs...)
			}
			return res, errors.Join(append(sourceErrs, fmt.Errorf("insert %s: %w", id, err))...)
		}
		res.Units += ytapi.CostInsert
		res.Inserted++
		log.Info("added", "video", id, "playlist", opts.PlaylistID)
	}

	return res, errors.Join(sourceErrs...)
}

// filterBand looks up the duration of every candidate and keeps the ones inside the band. Batched
// at the API's limit, because a per-video lookup costs the same unit each and there can be
// hundreds of candidates on a first run.
func filterBand(ctx context.Context, pl Playlist, candidates []string, opts Options, log *slog.Logger) ([]string, int, error) {
	var (
		keep  []string
		calls int
	)
	for chunk := range batches(candidates, ytapi.BatchSize) {
		durations, c, err := pl.Durations(ctx, chunk)
		calls += c
		if err != nil {
			return nil, calls, fmt.Errorf("read durations: %w", err)
		}
		for _, id := range chunk {
			d, ok := durations[id]
			if !ok {
				// Deleted, private or blocked between the feed and this call. Not an error, and
				// not something to add blind.
				log.Warn("video not returned by the api", "video", id)
				continue
			}
			if !InBand(d, opts.Min, opts.Max) {
				log.Debug("out of band", "video", id, "duration", d)
				continue
			}
			keep = append(keep, id)
		}
	}
	return keep, calls, nil
}

// batches splits ids into slices of at most n.
func batches(ids []string, n int) func(func([]string) bool) {
	return func(yield func([]string) bool) {
		for i := 0; i < len(ids); i += n {
			end := min(i+n, len(ids))
			if !yield(ids[i:end]) {
				return
			}
		}
	}
}
