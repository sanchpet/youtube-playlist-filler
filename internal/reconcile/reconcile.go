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
//
// Discovery and deduplication now both run through playlistItems.list — one endpoint, one failure
// mode, one thing to be right about.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sanchpet/youtube-playlist-filler/internal/ytapi"
)

// channelPages is how deep a normal run reads each channel's uploads playlist. One page is the
// fifty newest videos, which is weeks of headroom at any cadence these channels publish at, and
// makes the per-channel cost a single unit.
const channelPages = 1

// API is the YouTube surface a run needs. One interface, not two: discovering a channel's videos
// and reading the playlist they are compared against are the same call against different playlists,
// and the only thing that differs is how deep it reads.
type API interface {
	// PlaylistVideoIDs lists a playlist newest first, reading at most maxPages pages
	// (ytapi.AllPages for all of them), and reports the billed calls it made.
	PlaylistVideoIDs(ctx context.Context, playlistID string, maxPages int) (ids []string, calls int, err error)
	Durations(ctx context.Context, ids []string) (map[string]time.Duration, int, error)
	Insert(ctx context.Context, playlistID, videoID string) error
}

// Options is one run.
type Options struct {
	PlaylistID string
	Channels   []string

	// Min and Max are the closed duration band a video has to fall inside, and since discovery
	// moved to the uploads playlist they are the *only* thing keeping Shorts out — the Atom feed's
	// /shorts/ link hint went away with the feed, and an uploads playlist lists Shorts alongside
	// everything else with nothing to mark them. A ceiling-only filter is not a weaker version of
	// this: it is the defect that put 232 eleven-second Shorts into this playlist.
	Min, Max time.Duration

	// MaxInserts caps how many videos one run will add. It is a quota fuse, not a policy: at
	// fifty units an insert against a ten-thousand-unit day, an unbounded run that finds a
	// thousand candidates spends the entire budget in its first minutes and blinds every other
	// run until the reset.
	MaxInserts int

	DryRun bool

	// FullReconcile walks every page of every uploads playlist instead of just the newest one.
	// A normal run sees a bounded window and nothing tells it what fell out the back of that
	// window, so this is the pass that recovers anything a burst of publishing pushed past the
	// first fifty. It costs a unit per fifty videos per channel, which is why it is a weekly
	// schedule beside the normal one rather than the normal one.
	FullReconcile bool
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
// Keyed on the video id and nothing else — the one identity both sides of the comparison agree
// on. Publication timestamps are not: they differ between endpoints for the same video and change
// when a video is edited, so a key involving them would report everything as new on every run.
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
// this time — but the run still ends in an error, so a channel that has been failing for a week is
// visible rather than merely quiet.
func Run(ctx context.Context, api API, opts Options, log *slog.Logger) (Result, error) {
	var res Result
	res.DryRun = opts.DryRun

	// AllPages, always. A short read here is not a smaller answer, it is a wrong one: the ids it
	// failed to return are exactly the ids that would then be added a second time.
	present, calls, err := api.PlaylistVideoIDs(ctx, opts.PlaylistID, ytapi.AllPages)
	res.Units += calls * ytapi.CostList
	if err != nil {
		return res, fmt.Errorf("read playlist: %w", err)
	}
	res.PlaylistSize = len(present)
	inPlaylist := make(map[string]struct{}, len(present))
	for _, id := range present {
		inPlaylist[id] = struct{}{}
	}
	log.Info("playlist read", "playlist", opts.PlaylistID, "items", len(present),
		"distinct", len(inPlaylist), "pages", calls)

	pages := channelPages
	if opts.FullReconcile {
		pages = ytapi.AllPages
	}

	var (
		uploads    []string
		sourceErrs []error
	)
	for _, ch := range opts.Channels {
		ids, calls, err := discover(ctx, api, ch, pages)
		res.Units += calls * ytapi.CostList
		if err != nil {
			log.Error("channel unreadable", "channel", ch, "err", err)
			sourceErrs = append(sourceErrs, err)
			continue
		}
		log.Debug("channel read", "channel", ch, "videos", len(ids), "pages", calls)
		uploads = append(uploads, ids...)
	}

	candidates := Diff(uploads, inPlaylist)
	res.Candidates = len(candidates)
	log.Info("candidates", "uploads", len(uploads), "new", len(candidates))

	keep, calls, err := filterBand(ctx, api, candidates, opts, log)
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
		if err := api.Insert(ctx, opts.PlaylistID, id); err != nil {
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

// discover lists a channel's recent uploads through its auto-generated uploads playlist.
func discover(ctx context.Context, api API, channelID string, pages int) ([]string, int, error) {
	uploads, err := ytapi.UploadsPlaylistID(channelID)
	if err != nil {
		return nil, 0, err
	}
	ids, calls, err := api.PlaylistVideoIDs(ctx, uploads, pages)
	if err != nil {
		return nil, calls, fmt.Errorf("channel %s: %w", channelID, err)
	}
	return ids, calls, nil
}

// filterBand looks up the duration of every candidate and keeps the ones inside the band. Batched
// at the API's limit, because a per-video lookup costs the same unit each and there can be
// hundreds of candidates on a full pass.
func filterBand(ctx context.Context, api API, candidates []string, opts Options, log *slog.Logger) ([]string, int, error) {
	var (
		keep  []string
		calls int
	)
	for chunk := range batches(candidates, ytapi.BatchSize) {
		durations, c, err := api.Durations(ctx, chunk)
		calls += c
		if err != nil {
			return nil, calls, fmt.Errorf("read durations: %w", err)
		}
		for _, id := range chunk {
			d, ok := durations[id]
			if !ok {
				// Deleted, private or blocked between being listed and this call. Not an error,
				// and not something to add blind.
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
