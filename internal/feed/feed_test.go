package feed_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sanchpet/youtube-playlist-filler/internal/feed"
)

func parseFixture(t *testing.T, name string) []string {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })

	ids, err := feed.Parse(f)
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return ids
}

// A real feed carries exactly 15 entries; one of them here is a Short, and a Short is the video
// this whole filter chain exists to keep out.
func TestParseDropsShorts(t *testing.T) {
	ids := parseFixture(t, "channel.xml")

	if got, want := len(ids), 14; got != want {
		t.Errorf("got %d ids, want %d (15 entries, one of them a Short)", got, want)
	}
	if slices.Contains(ids, "shortsAAA1") {
		t.Error("the /shorts/ entry survived parsing")
	}
	if ids[0] != "aaaaaaaaaa1" {
		t.Errorf("feed order not preserved: first id is %q", ids[0])
	}
}

// The same video appears twice with different published and updated stamps, which is exactly how
// YouTube re-announces an edited upload. Keyed on the id, that is one video.
func TestParseKeysOnVideoIDNotTimestamps(t *testing.T) {
	ids := parseFixture(t, "duplicate.xml")

	if want := []string{"dupdupdup1"}; !slices.Equal(ids, want) {
		t.Errorf("got %v, want %v", ids, want)
	}
}

// An entry whose videoId is not in the yt namespace means the feed's shape changed. Reconciling
// nothing looks identical to a quiet week, so it has to be an error rather than an empty result.
func TestParseRejectsEntryWithoutVideoID(t *testing.T) {
	f, err := os.Open("testdata/no-videoid.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	if _, err := feed.Parse(f); err == nil {
		t.Fatal("want an error for an entry carrying no yt:videoId, got nil")
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := feed.Parse(strings.NewReader("<feed><entry>")); err == nil {
		t.Fatal("want an error for a truncated document, got nil")
	}
}

func TestURL(t *testing.T) {
	const want = "https://www.youtube.com/feeds/videos.xml?channel_id=UCoH2qJSyODQpBKsK63Moc6Q"
	if got := feed.URL("UCoH2qJSyODQpBKsK63Moc6Q"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
