package reconcile_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/sanchpet/youtube-playlist-filler/internal/reconcile"
	"github.com/sanchpet/youtube-playlist-filler/internal/ytapi"
)

const (
	minD = 15 * time.Minute
	maxD = 3 * time.Hour
)

// Both bounds have to reject, and the floor is the one with history: a filter that only enforced
// the ceiling let eleven-second Shorts into the playlist, because every Short is comfortably under
// three hours. The band exists for the lower bound; the upper one is the easy half.
func TestInBandRejectsAtBothEnds(t *testing.T) {
	cases := []struct {
		name string
		d    time.Duration
		want bool
	}{
		{"eleven-second short", 11 * time.Second, false},
		{"two-minute teaser", 2 * time.Minute, false},
		{"a second under the floor", minD - time.Second, false},
		{"exactly the floor", minD, true},
		{"a second over the floor", minD + time.Second, true},
		{"an hour", time.Hour, true},
		{"a second under the ceiling", maxD - time.Second, true},
		{"exactly the ceiling", maxD, true},
		{"a second over the ceiling", maxD + time.Second, false},
		{"an eight-hour upload", 8 * time.Hour, false},
		{"zero", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reconcile.InBand(c.d, minD, maxD); got != c.want {
				t.Errorf("InBand(%s, %s, %s) = %v, want %v", c.d, minD, maxD, got, c.want)
			}
		})
	}
}

func TestDiff(t *testing.T) {
	present := map[string]struct{}{"old1": {}, "old2": {}}

	got := reconcile.Diff([]string{"new1", "old1", "new2", "new1", "old2", "new3"}, present)

	if want := []string{"new1", "new2", "new3"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDiffOfAnAlreadyReconciledPlaylistIsEmpty(t *testing.T) {
	present := map[string]struct{}{"a": {}, "b": {}}

	if got := reconcile.Diff([]string{"a", "b"}, present); len(got) != 0 {
		t.Errorf("got %v, want nothing: most runs are no-ops", got)
	}
}

// fakePlaylist is the API, minus the network. It records inserts so the test can prove the
// service, not the API, is what stops a video being added twice.
type fakePlaylist struct {
	items     []string
	durations map[string]time.Duration
	inserted  []string

	listCalls     int
	durationCalls int
	insertErr     func(videoID string) error
	listErr       error
}

func (f *fakePlaylist) PlaylistVideoIDs(context.Context, string) ([]string, int, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, 1, f.listErr
	}
	return slices.Clone(f.items), 1, nil
}

func (f *fakePlaylist) Durations(_ context.Context, ids []string) (map[string]time.Duration, int, error) {
	f.durationCalls++
	if len(ids) > ytapi.BatchSize {
		return nil, 1, fmt.Errorf("batch of %d exceeds %d", len(ids), ytapi.BatchSize)
	}
	out := make(map[string]time.Duration, len(ids))
	for _, id := range ids {
		if d, ok := f.durations[id]; ok {
			out[id] = d
		}
	}
	return out, 1, nil
}

func (f *fakePlaylist) Insert(_ context.Context, _, videoID string) error {
	if f.insertErr != nil {
		if err := f.insertErr(videoID); err != nil {
			return err
		}
	}
	// The real API is not idempotent: it would append this id whether or not it is already there.
	f.items = append(f.items, videoID)
	f.inserted = append(f.inserted, videoID)
	return nil
}

type fakeSource struct {
	byChannel map[string][]string
	calls     int
	err       error
}

func (f *fakeSource) VideoIDs(_ context.Context, channelID string) ([]string, int, error) {
	if f.err != nil {
		return nil, f.calls, f.err
	}
	return f.byChannel[channelID], f.calls, nil
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func opts(o reconcile.Options) reconcile.Options {
	o.PlaylistID = "PLtest"
	if o.Min == 0 && o.Max == 0 {
		o.Min, o.Max = minD, maxD
	}
	if o.MaxInserts == 0 {
		o.MaxInserts = 100
	}
	return o
}

func TestRunAddsOnlyNewInBandVideos(t *testing.T) {
	pl := &fakePlaylist{
		items: []string{"already1"},
		durations: map[string]time.Duration{
			"already1": time.Hour,
			"long1":    2 * time.Hour,
			"short1":   11 * time.Second,
			"huge1":    8 * time.Hour,
			"long2":    minD,
		},
	}
	src := &fakeSource{byChannel: map[string][]string{
		"UC1": {"long1", "short1", "already1"},
		"UC2": {"huge1", "long2"},
	}}

	res, err := reconcile.Run(t.Context(), pl, src, opts(reconcile.Options{
		Channels: []string{"UC1", "UC2"},
	}), discardLog())
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"long1", "long2"}; !slices.Equal(pl.inserted, want) {
		t.Errorf("inserted %v, want %v", pl.inserted, want)
	}
	if res.Candidates != 4 {
		t.Errorf("candidates = %d, want 4 (already1 is in the playlist)", res.Candidates)
	}
	if res.InBand != 2 {
		t.Errorf("in band = %d, want 2", res.InBand)
	}
	// One list page + one videos.list batch + two inserts.
	if want := 2*ytapi.CostList + 2*ytapi.CostInsert; res.Units != want {
		t.Errorf("units = %d, want %d", res.Units, want)
	}
}

// The pre-read is the whole deduplication mechanism. Skipping it, or keying the diff on anything
// the feed and the API disagree about, gives a playlist that grows a second copy of every video on
// every run — and the API reports every one of those inserts as a success.
func TestRunIsIdempotentAcrossRuns(t *testing.T) {
	durations := map[string]time.Duration{"long1": time.Hour, "long2": 90 * time.Minute}
	pl := &fakePlaylist{durations: durations}
	src := &fakeSource{byChannel: map[string][]string{"UC1": {"long1", "long2"}}}
	o := opts(reconcile.Options{Channels: []string{"UC1"}})

	for run := 1; run <= 3; run++ {
		if _, err := reconcile.Run(t.Context(), pl, src, o, discardLog()); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}

	if want := []string{"long1", "long2"}; !slices.Equal(pl.inserted, want) {
		t.Errorf("after three runs the playlist got %v, want %v", pl.inserted, want)
	}
}

func TestRunDryRunInsertsNothing(t *testing.T) {
	pl := &fakePlaylist{durations: map[string]time.Duration{"long1": time.Hour}}
	src := &fakeSource{byChannel: map[string][]string{"UC1": {"long1"}}}

	res, err := reconcile.Run(t.Context(), pl, src, opts(reconcile.Options{
		Channels: []string{"UC1"},
		DryRun:   true,
	}), discardLog())
	if err != nil {
		t.Fatal(err)
	}

	if len(pl.inserted) != 0 {
		t.Errorf("dry run inserted %v", pl.inserted)
	}
	if res.Inserted != 1 {
		t.Errorf("reported %d would-be inserts, want 1", res.Inserted)
	}
	if res.Units != 2*ytapi.CostList {
		t.Errorf("units = %d, want %d: a dry run spends nothing on inserts", res.Units, 2*ytapi.CostList)
	}
}

func TestRunHonoursTheInsertCap(t *testing.T) {
	durations := map[string]time.Duration{}
	var ids []string
	for i := range 10 {
		id := fmt.Sprintf("vid%02d", i)
		ids = append(ids, id)
		durations[id] = time.Hour
	}
	pl := &fakePlaylist{durations: durations}
	src := &fakeSource{byChannel: map[string][]string{"UC1": ids}}

	res, err := reconcile.Run(t.Context(), pl, src, opts(reconcile.Options{
		Channels:   []string{"UC1"},
		MaxInserts: 3,
	}), discardLog())
	if err != nil {
		t.Fatal(err)
	}

	if len(pl.inserted) != 3 {
		t.Errorf("inserted %d, want 3", len(pl.inserted))
	}
	if res.Deferred != 7 {
		t.Errorf("deferred = %d, want 7", res.Deferred)
	}
}

// A full playlist is terminal but not a failure: nothing about retrying or waiting changes it, and
// a crash loop would bury the one line that says what happened.
func TestRunStopsCleanlyOnAFullPlaylist(t *testing.T) {
	pl := &fakePlaylist{
		durations: map[string]time.Duration{"long1": time.Hour, "long2": time.Hour, "long3": time.Hour},
		insertErr: func(videoID string) error {
			if videoID == "long2" {
				return fmt.Errorf("playlistItems.insert: %w: 403", ytapi.ErrPlaylistFull)
			}
			return nil
		},
	}
	src := &fakeSource{byChannel: map[string][]string{"UC1": {"long1", "long2", "long3"}}}

	res, err := reconcile.Run(t.Context(), pl, src, opts(reconcile.Options{
		Channels: []string{"UC1"},
	}), discardLog())

	if err != nil {
		t.Fatalf("a full playlist must not fail the run: %v", err)
	}
	if !res.PlaylistFull {
		t.Error("result does not record that the playlist is full")
	}
	if want := []string{"long1"}; !slices.Equal(pl.inserted, want) {
		t.Errorf("inserted %v, want %v: the run stops at the refusal", pl.inserted, want)
	}
	if res.Deferred != 2 {
		t.Errorf("deferred = %d, want 2", res.Deferred)
	}
}

func TestRunFailsOnQuotaExceeded(t *testing.T) {
	pl := &fakePlaylist{
		durations: map[string]time.Duration{"long1": time.Hour},
		insertErr: func(string) error {
			return fmt.Errorf("playlistItems.insert: %w: 403", ytapi.ErrQuotaExceeded)
		},
	}
	src := &fakeSource{byChannel: map[string][]string{"UC1": {"long1"}}}

	_, err := reconcile.Run(t.Context(), pl, src, opts(reconcile.Options{
		Channels: []string{"UC1"},
	}), discardLog())

	if !errors.Is(err, ytapi.ErrQuotaExceeded) {
		t.Fatalf("got %v, want the run to fail with ErrQuotaExceeded", err)
	}
}

// A playlist that cannot be read is not a playlist with nothing in it. Continuing would treat
// every video on every channel as new.
func TestRunRefusesToProceedWithoutThePreRead(t *testing.T) {
	pl := &fakePlaylist{listErr: errors.New("network is unreachable")}
	src := &fakeSource{byChannel: map[string][]string{"UC1": {"long1"}}}

	_, err := reconcile.Run(t.Context(), pl, src, opts(reconcile.Options{
		Channels: []string{"UC1"},
	}), discardLog())

	if err == nil {
		t.Fatal("want the run to fail when the playlist cannot be read")
	}
	if len(pl.inserted) != 0 {
		t.Errorf("inserted %v without knowing what is already there", pl.inserted)
	}
}

// One unreadable channel costs that channel's videos, not the run — but the run still ends in an
// error, so a feed that has been failing all week is visible rather than merely quiet.
func TestRunReportsAnUnreadableChannelAndKeepsGoing(t *testing.T) {
	pl := &fakePlaylist{durations: map[string]time.Duration{"long1": time.Hour}}
	src := &fakeSource{err: errors.New("feed returned 404 Not Found")}

	_, err := reconcile.Run(t.Context(), pl, src, opts(reconcile.Options{
		Channels: []string{"UC1", "UC2"},
	}), discardLog())

	if err == nil {
		t.Fatal("want the run to end in an error")
	}
}

// videos.list takes at most fifty ids. Sending fifty-one is a 400 for the whole batch, so the
// batching is the difference between a first run working and a first run failing.
func TestRunBatchesDurationLookups(t *testing.T) {
	durations := map[string]time.Duration{}
	var ids []string
	for i := range 120 {
		id := fmt.Sprintf("vid%03d", i)
		ids = append(ids, id)
		durations[id] = time.Hour
	}
	pl := &fakePlaylist{durations: durations}
	src := &fakeSource{byChannel: map[string][]string{"UC1": ids}}

	res, err := reconcile.Run(t.Context(), pl, src, opts(reconcile.Options{
		Channels:   []string{"UC1"},
		MaxInserts: 1000,
	}), discardLog())
	if err != nil {
		t.Fatal(err)
	}

	if pl.durationCalls != 3 {
		t.Errorf("made %d videos.list calls for 120 ids, want 3", pl.durationCalls)
	}
	if res.InBand != 120 {
		t.Errorf("in band = %d, want 120", res.InBand)
	}
}

// A video the API does not return — deleted, private, region-blocked between the feed and the
// lookup — has no duration, and a missing duration must not read as zero.
func TestRunSkipsVideosTheAPIDoesNotReturn(t *testing.T) {
	pl := &fakePlaylist{durations: map[string]time.Duration{"long1": time.Hour}}
	src := &fakeSource{byChannel: map[string][]string{"UC1": {"long1", "vanished"}}}

	res, err := reconcile.Run(t.Context(), pl, src, opts(reconcile.Options{
		Channels: []string{"UC1"},
	}), discardLog())
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"long1"}; !slices.Equal(pl.inserted, want) {
		t.Errorf("inserted %v, want %v", pl.inserted, want)
	}
	if res.InBand != 1 {
		t.Errorf("in band = %d, want 1", res.InBand)
	}
}
