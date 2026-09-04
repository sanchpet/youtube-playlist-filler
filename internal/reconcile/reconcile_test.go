package reconcile_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sanchpet/youtube-playlist-filler/internal/reconcile"
	"github.com/sanchpet/youtube-playlist-filler/internal/ytapi"
)

const (
	minD = 15 * time.Minute
	maxD = 3 * time.Hour
)

// Both bounds have to reject, and since discovery moved to the uploads playlist the floor is the
// only thing keeping Shorts out at all — an uploads playlist lists them with nothing to mark them.
// A filter enforcing only the ceiling admits an eleven-second Short, because eleven seconds is
// comfortably under three hours. That is not a hypothetical: it put 232 of them in the playlist.
func TestInBandRejectsAtBothEnds(t *testing.T) {
	cases := []struct {
		name string
		d    time.Duration
		want bool
	}{
		{"eleven-second short", 11 * time.Second, false},
		{"fifteen-second short", 15 * time.Second, false},
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

// fakeAPI is the API, minus the network: playlists as pages of fifty, so the page bound the two
// schedules differ by is actually exercised. It records inserts, so a test can prove the service —
// not the API — is what stops a video being added twice.
type fakeAPI struct {
	playlists map[string][]string
	durations map[string]time.Duration
	inserted  []string

	listed        map[string]int // playlist id -> pages served
	durationCalls int
	insertErr     func(videoID string) error
	listErr       error
}

const fakePageSize = 50

func (f *fakeAPI) PlaylistVideoIDs(_ context.Context, playlistID string, maxPages int) ([]string, int, error) {
	if f.listErr != nil {
		return nil, 1, f.listErr
	}
	if f.listed == nil {
		f.listed = map[string]int{}
	}

	all := f.playlists[playlistID]
	var (
		ids   []string
		pages int
	)
	for start := 0; ; start += fakePageSize {
		end := min(start+fakePageSize, len(all))
		ids = append(ids, all[start:end]...)
		pages++
		if end >= len(all) || (maxPages != ytapi.AllPages && pages >= maxPages) {
			break
		}
	}
	f.listed[playlistID] += pages
	return ids, pages, nil
}

func (f *fakeAPI) Durations(_ context.Context, ids []string) (map[string]time.Duration, int, error) {
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

func (f *fakeAPI) Insert(_ context.Context, playlistID, videoID string) error {
	if f.insertErr != nil {
		if err := f.insertErr(videoID); err != nil {
			return err
		}
	}
	// The real API is not idempotent: it appends whether or not the video is already there.
	f.playlists[playlistID] = append(f.playlists[playlistID], videoID)
	f.inserted = append(f.inserted, videoID)
	return nil
}

func newFake(target []string) *fakeAPI {
	return &fakeAPI{
		playlists: map[string][]string{"PLtest": target},
		durations: map[string]time.Duration{},
	}
}

// channel loads a channel's uploads playlist into the fake under the UU id the reconciler will
// derive, which is also how the test proves it derives it.
func (f *fakeAPI) channel(channelID string, ids []string, d time.Duration) {
	uploads, err := ytapi.UploadsPlaylistID(channelID)
	if err != nil {
		panic(err)
	}
	f.playlists[uploads] = ids
	for _, id := range ids {
		if _, ok := f.durations[id]; !ok {
			f.durations[id] = d
		}
	}
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

// A real page of a real uploads playlist, with the real distribution: mostly Shorts. Nothing in
// the listing distinguishes them — the durations are the only signal there is.
func loadUploadsFixture(t *testing.T) (ids []string, durations map[string]time.Duration) {
	t.Helper()

	var page struct {
		Items []struct {
			ContentDetails struct {
				VideoID string `json:"videoId"`
			} `json:"contentDetails"`
		} `json:"items"`
	}
	var videos struct {
		Items []struct {
			ID             string `json:"id"`
			ContentDetails struct {
				Duration string `json:"duration"`
			} `json:"contentDetails"`
		} `json:"items"`
	}
	for name, into := range map[string]any{"uploads-page.json": &page, "durations.json": &videos} {
		raw, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	for _, it := range page.Items {
		ids = append(ids, it.ContentDetails.VideoID)
	}
	durations = make(map[string]time.Duration, len(videos.Items))
	for _, v := range videos.Items {
		d, err := ytapi.ParseDuration(v.ContentDetails.Duration)
		if err != nil {
			t.Fatalf("fixture duration %q: %v", v.ContentDetails.Duration, err)
		}
		durations[v.ID] = d
	}
	if len(ids) != len(durations) {
		t.Fatalf("fixture mismatch: %d listed, %d durations", len(ids), len(durations))
	}
	return ids, durations
}

// The end-to-end shape of the defect the band exists for, against a realistic page: 34 Shorts of
// 11-15 seconds and 3 uploads past the ceiling have to be left behind, and every one of the 13
// long-form videos taken.
func TestRunKeepsShortsOutOfARealUploadsPage(t *testing.T) {
	ids, durations := loadUploadsFixture(t)

	api := newFake(nil)
	api.durations = durations
	api.channel("UCoH2qJSyODQpBKsK63Moc6Q", ids, 0)

	res, err := reconcile.Run(t.Context(), api, opts(reconcile.Options{
		Channels: []string{"UCoH2qJSyODQpBKsK63Moc6Q"},
	}), discardLog())
	if err != nil {
		t.Fatal(err)
	}

	var shorts, huge int
	for _, id := range api.inserted {
		switch {
		case strings.HasPrefix(id, "SHR"):
			shorts++
		case strings.HasPrefix(id, "HUG"):
			huge++
		}
	}
	if shorts != 0 {
		t.Errorf("%d Shorts reached the playlist; the lower bound is the only thing that stops them", shorts)
	}
	if huge != 0 {
		t.Errorf("%d over-length uploads reached the playlist", huge)
	}
	if want := 13; res.InBand != want || len(api.inserted) != want {
		t.Errorf("in band = %d, inserted = %d, want %d of the 50 listed", res.InBand, len(api.inserted), want)
	}
}

func TestRunAddsOnlyNewInBandVideos(t *testing.T) {
	api := newFake([]string{"already1"})
	api.durations = map[string]time.Duration{
		"already1": time.Hour,
		"long1":    2 * time.Hour,
		"short1":   11 * time.Second,
		"huge1":    8 * time.Hour,
		"long2":    minD,
	}
	api.channel("UC1aaaaaaaaaaaaaaaaaaaaa", []string{"long1", "short1", "already1"}, 0)
	api.channel("UC2bbbbbbbbbbbbbbbbbbbbb", []string{"huge1", "long2"}, 0)

	res, err := reconcile.Run(t.Context(), api, opts(reconcile.Options{
		Channels: []string{"UC1aaaaaaaaaaaaaaaaaaaaa", "UC2bbbbbbbbbbbbbbbbbbbbb"},
	}), discardLog())
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"long1", "long2"}; !slices.Equal(api.inserted, want) {
		t.Errorf("inserted %v, want %v", api.inserted, want)
	}
	if res.Candidates != 4 {
		t.Errorf("candidates = %d, want 4 (already1 is in the playlist)", res.Candidates)
	}
	if res.InBand != 2 {
		t.Errorf("in band = %d, want 2", res.InBand)
	}
	// One page of the target playlist, one page per channel, one videos.list, two inserts.
	if want := 4*ytapi.CostList + 2*ytapi.CostInsert; res.Units != want {
		t.Errorf("units = %d, want %d", res.Units, want)
	}
}

// Discovery goes through the channel's auto-generated uploads playlist, whose id is the channel id
// with UC swapped for UU. Getting that wrong reads an empty or foreign playlist and finds nothing,
// which looks exactly like a channel that has not published.
func TestRunDiscoversThroughTheUploadsPlaylist(t *testing.T) {
	api := newFake(nil)
	api.durations = map[string]time.Duration{"long1": time.Hour}
	api.playlists["UUoH2qJSyODQpBKsK63Moc6Q"] = []string{"long1"}

	if _, err := reconcile.Run(t.Context(), api, opts(reconcile.Options{
		Channels: []string{"UCoH2qJSyODQpBKsK63Moc6Q"},
	}), discardLog()); err != nil {
		t.Fatal(err)
	}

	if want := []string{"long1"}; !slices.Equal(api.inserted, want) {
		t.Errorf("inserted %v, want %v", api.inserted, want)
	}
}

func TestRunRejectsAChannelIDItCannotDerive(t *testing.T) {
	api := newFake(nil)

	_, err := reconcile.Run(t.Context(), api, opts(reconcile.Options{
		Channels: []string{"PLnotachannel"},
	}), discardLog())

	if err == nil {
		t.Fatal("want an error for an id that is not a channel")
	}
}

// The two schedules differ by exactly this: how deep each channel is read. A normal run takes the
// newest page; the weekly pass walks the lot.
func TestRunReadsOnePageOfEachChannelByDefault(t *testing.T) {
	var deep []string
	for i := range 130 {
		deep = append(deep, fmt.Sprintf("vid%03d", i))
	}
	api := newFake(nil)
	api.channel("UCoH2qJSyODQpBKsK63Moc6Q", deep, time.Hour)

	res, err := reconcile.Run(t.Context(), api, opts(reconcile.Options{
		Channels:   []string{"UCoH2qJSyODQpBKsK63Moc6Q"},
		MaxInserts: 1000,
	}), discardLog())
	if err != nil {
		t.Fatal(err)
	}

	if got := api.listed["UUoH2qJSyODQpBKsK63Moc6Q"]; got != 1 {
		t.Errorf("read %d pages of the uploads playlist, want 1", got)
	}
	if res.Candidates != 50 {
		t.Errorf("candidates = %d, want the newest 50", res.Candidates)
	}
}

func TestFullReconcileWalksEveryPage(t *testing.T) {
	var deep []string
	for i := range 130 {
		deep = append(deep, fmt.Sprintf("vid%03d", i))
	}
	api := newFake(nil)
	api.channel("UCoH2qJSyODQpBKsK63Moc6Q", deep, time.Hour)

	res, err := reconcile.Run(t.Context(), api, opts(reconcile.Options{
		Channels:      []string{"UCoH2qJSyODQpBKsK63Moc6Q"},
		MaxInserts:    1000,
		FullReconcile: true,
	}), discardLog())
	if err != nil {
		t.Fatal(err)
	}

	if got := api.listed["UUoH2qJSyODQpBKsK63Moc6Q"]; got != 3 {
		t.Errorf("read %d pages of the uploads playlist, want 3", got)
	}
	if res.Candidates != 130 {
		t.Errorf("candidates = %d, want all 130", res.Candidates)
	}
}

// The target playlist is always read whole, on both schedules. A bounded read of it would leave
// the ids it did not reach looking absent — and they would be added again.
func TestRunAlwaysReadsTheWholeTargetPlaylist(t *testing.T) {
	var existing []string
	for i := range 120 {
		existing = append(existing, fmt.Sprintf("have%03d", i))
	}
	api := newFake(existing)
	api.durations = map[string]time.Duration{"have119": time.Hour}
	api.channel("UCoH2qJSyODQpBKsK63Moc6Q", []string{"have119"}, 0)

	res, err := reconcile.Run(t.Context(), api, opts(reconcile.Options{
		Channels: []string{"UCoH2qJSyODQpBKsK63Moc6Q"},
	}), discardLog())
	if err != nil {
		t.Fatal(err)
	}

	if api.listed["PLtest"] != 3 {
		t.Errorf("read %d pages of the target playlist, want all 3", api.listed["PLtest"])
	}
	if res.PlaylistSize != 120 {
		t.Errorf("playlist size = %d, want 120", res.PlaylistSize)
	}
	if len(api.inserted) != 0 {
		t.Errorf("re-added %v, which is on the last page of the playlist", api.inserted)
	}
}

// The pre-read is the whole deduplication mechanism. Skipping it, or keying the diff on anything
// the two endpoints disagree about, gives a playlist that grows a second copy of every video on
// every run — and the API reports every one of those inserts as a success.
func TestRunIsIdempotentAcrossRuns(t *testing.T) {
	api := newFake(nil)
	api.channel("UCoH2qJSyODQpBKsK63Moc6Q", []string{"long1", "long2"}, time.Hour)
	o := opts(reconcile.Options{Channels: []string{"UCoH2qJSyODQpBKsK63Moc6Q"}})

	for run := 1; run <= 3; run++ {
		if _, err := reconcile.Run(t.Context(), api, o, discardLog()); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}

	if want := []string{"long1", "long2"}; !slices.Equal(api.inserted, want) {
		t.Errorf("after three runs the playlist got %v, want %v", api.inserted, want)
	}
}

func TestRunDryRunInsertsNothing(t *testing.T) {
	api := newFake(nil)
	api.channel("UCoH2qJSyODQpBKsK63Moc6Q", []string{"long1"}, time.Hour)

	res, err := reconcile.Run(t.Context(), api, opts(reconcile.Options{
		Channels: []string{"UCoH2qJSyODQpBKsK63Moc6Q"},
		DryRun:   true,
	}), discardLog())
	if err != nil {
		t.Fatal(err)
	}

	if len(api.inserted) != 0 {
		t.Errorf("dry run inserted %v", api.inserted)
	}
	if res.Inserted != 1 {
		t.Errorf("reported %d would-be inserts, want 1", res.Inserted)
	}
	if want := 3 * ytapi.CostList; res.Units != want {
		t.Errorf("units = %d, want %d: a dry run spends nothing on inserts", res.Units, want)
	}
}

func TestRunHonoursTheInsertCap(t *testing.T) {
	var ids []string
	for i := range 10 {
		ids = append(ids, fmt.Sprintf("vid%02d", i))
	}
	api := newFake(nil)
	api.channel("UCoH2qJSyODQpBKsK63Moc6Q", ids, time.Hour)

	res, err := reconcile.Run(t.Context(), api, opts(reconcile.Options{
		Channels:   []string{"UCoH2qJSyODQpBKsK63Moc6Q"},
		MaxInserts: 3,
	}), discardLog())
	if err != nil {
		t.Fatal(err)
	}

	if len(api.inserted) != 3 {
		t.Errorf("inserted %d, want 3", len(api.inserted))
	}
	if res.Deferred != 7 {
		t.Errorf("deferred = %d, want 7", res.Deferred)
	}
}

// A full playlist is terminal but not a failure: nothing about retrying or waiting changes it, and
// a crash loop would bury the one line that says what happened.
func TestRunStopsCleanlyOnAFullPlaylist(t *testing.T) {
	api := newFake(nil)
	api.channel("UCoH2qJSyODQpBKsK63Moc6Q", []string{"long1", "long2", "long3"}, time.Hour)
	api.insertErr = func(videoID string) error {
		if videoID == "long2" {
			return fmt.Errorf("playlistItems.insert: %w: 403", ytapi.ErrPlaylistFull)
		}
		return nil
	}

	res, err := reconcile.Run(t.Context(), api, opts(reconcile.Options{
		Channels: []string{"UCoH2qJSyODQpBKsK63Moc6Q"},
	}), discardLog())

	if err != nil {
		t.Fatalf("a full playlist must not fail the run: %v", err)
	}
	if !res.PlaylistFull {
		t.Error("result does not record that the playlist is full")
	}
	if want := []string{"long1"}; !slices.Equal(api.inserted, want) {
		t.Errorf("inserted %v, want %v: the run stops at the refusal", api.inserted, want)
	}
	if res.Deferred != 2 {
		t.Errorf("deferred = %d, want 2", res.Deferred)
	}
}

func TestRunFailsOnQuotaExceeded(t *testing.T) {
	api := newFake(nil)
	api.channel("UCoH2qJSyODQpBKsK63Moc6Q", []string{"long1"}, time.Hour)
	api.insertErr = func(string) error {
		return fmt.Errorf("playlistItems.insert: %w: 403", ytapi.ErrQuotaExceeded)
	}

	_, err := reconcile.Run(t.Context(), api, opts(reconcile.Options{
		Channels: []string{"UCoH2qJSyODQpBKsK63Moc6Q"},
	}), discardLog())

	if !errors.Is(err, ytapi.ErrQuotaExceeded) {
		t.Fatalf("got %v, want the run to fail with ErrQuotaExceeded", err)
	}
}

// A playlist that cannot be read is not a playlist with nothing in it. Continuing would treat
// every video on every channel as new — and discovery now runs through the same endpoint, so the
// call that fails here is the call the whole run depends on twice over.
func TestRunRefusesToProceedWithoutThePreRead(t *testing.T) {
	api := newFake(nil)
	api.listErr = errors.New("network is unreachable")
	api.channel("UCoH2qJSyODQpBKsK63Moc6Q", []string{"long1"}, time.Hour)

	_, err := reconcile.Run(t.Context(), api, opts(reconcile.Options{
		Channels: []string{"UCoH2qJSyODQpBKsK63Moc6Q"},
	}), discardLog())

	if err == nil {
		t.Fatal("want the run to fail when the playlist cannot be read")
	}
	if len(api.inserted) != 0 {
		t.Errorf("inserted %v without knowing what is already there", api.inserted)
	}
}

// One unreadable channel costs that channel's videos, not the run — but the run still ends in an
// error, so a channel that has been failing all week is visible rather than merely quiet.
func TestRunReportsAnUnreadableChannelAndKeepsGoing(t *testing.T) {
	api := &failingChannels{fakeAPI: newFake(nil), bad: "UU1aaaaaaaaaaaaaaaaaaaaa"}
	api.channel("UC1aaaaaaaaaaaaaaaaaaaaa", []string{"lost1"}, time.Hour)
	api.channel("UC2bbbbbbbbbbbbbbbbbbbbb", []string{"long1"}, time.Hour)

	res, err := reconcile.Run(t.Context(), api, opts(reconcile.Options{
		Channels: []string{"UC1aaaaaaaaaaaaaaaaaaaaa", "UC2bbbbbbbbbbbbbbbbbbbbb"},
	}), discardLog())

	if err == nil {
		t.Fatal("want the run to end in an error")
	}
	if want := []string{"long1"}; !slices.Equal(api.inserted, want) {
		t.Errorf("inserted %v, want %v: the readable channel is still reconciled", api.inserted, want)
	}
	if res.Inserted != 1 {
		t.Errorf("inserted = %d, want 1", res.Inserted)
	}
}

// failingChannels fails one playlist and serves the rest, which the plain fake cannot express.
type failingChannels struct {
	*fakeAPI
	bad string
}

func (f *failingChannels) PlaylistVideoIDs(ctx context.Context, playlistID string, maxPages int) ([]string, int, error) {
	if playlistID == f.bad {
		return nil, 1, errors.New("playlistNotFound")
	}
	return f.fakeAPI.PlaylistVideoIDs(ctx, playlistID, maxPages)
}

// videos.list takes at most fifty ids. Sending fifty-one is a 400 for the whole batch, so the
// batching is the difference between a full pass working and a full pass failing.
func TestRunBatchesDurationLookups(t *testing.T) {
	var ids []string
	for i := range 120 {
		ids = append(ids, fmt.Sprintf("vid%03d", i))
	}
	api := newFake(nil)
	api.channel("UCoH2qJSyODQpBKsK63Moc6Q", ids, time.Hour)

	res, err := reconcile.Run(t.Context(), api, opts(reconcile.Options{
		Channels:      []string{"UCoH2qJSyODQpBKsK63Moc6Q"},
		MaxInserts:    1000,
		FullReconcile: true,
	}), discardLog())
	if err != nil {
		t.Fatal(err)
	}

	if api.durationCalls != 3 {
		t.Errorf("made %d videos.list calls for 120 ids, want 3", api.durationCalls)
	}
	if res.InBand != 120 {
		t.Errorf("in band = %d, want 120", res.InBand)
	}
}

// A video the API does not return — deleted, private, region-blocked between the listing and the
// lookup — has no duration, and a missing duration must not read as zero.
func TestRunSkipsVideosTheAPIDoesNotReturn(t *testing.T) {
	api := newFake(nil)
	api.channel("UCoH2qJSyODQpBKsK63Moc6Q", []string{"long1", "vanished"}, time.Hour)
	delete(api.durations, "vanished")

	res, err := reconcile.Run(t.Context(), api, opts(reconcile.Options{
		Channels: []string{"UCoH2qJSyODQpBKsK63Moc6Q"},
	}), discardLog())
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"long1"}; !slices.Equal(api.inserted, want) {
		t.Errorf("inserted %v, want %v", api.inserted, want)
	}
	if res.InBand != 1 {
		t.Errorf("in band = %d, want 1", res.InBand)
	}
}
