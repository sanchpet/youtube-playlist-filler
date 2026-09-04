// Package feed reads a channel's recent uploads from its public Atom feed.
//
// The feed is free — it costs no API quota and needs no credential — which is the only reason the
// hourly run is affordable at all. What it buys is bounded: exactly 15 entries, newest first, with
// no watermark and no way to ask for more. Anything a channel published beyond those 15 since the
// last run is simply not in it, which is what the uploads-playlist path exists to recover.
package feed

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ytNS is the namespace carrying the fields YouTube adds to Atom. Matching on it rather than on
// the bare local name keeps the parse from binding to an element some other namespace may add.
const ytNS = "http://www.youtube.com/xml/schemas/2015"

// maxBody caps what is read from one feed. A feed is a few tens of kilobytes; anything past this
// is not a feed, and reading it to the end would be the only unbounded allocation in the run.
const maxBody = 4 << 20

type atomFeed struct {
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	VideoID string     `xml:"http://www.youtube.com/xml/schemas/2015 videoId"`
	Links   []atomLink `xml:"link"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
}

// Parse reads an Atom feed and returns its video ids, newest first, with Shorts removed.
//
// Keyed on the video id alone. The published and updated timestamps differ between the feed and
// the API for the same video — and change on their own when a video is edited — so any identity
// built on them would re-add videos that are already in the playlist.
func Parse(r io.Reader) ([]string, error) {
	var f atomFeed
	if err := xml.NewDecoder(io.LimitReader(r, maxBody)).Decode(&f); err != nil {
		return nil, fmt.Errorf("decode feed: %w", err)
	}

	ids := make([]string, 0, len(f.Entries))
	seen := make(map[string]struct{}, len(f.Entries))
	for i, e := range f.Entries {
		id := strings.TrimSpace(e.VideoID)
		if id == "" {
			// Not a video we failed to want — a feed whose shape stopped matching. Reconciling
			// nothing is the failure this would otherwise hide, so it is refused out loud.
			return nil, fmt.Errorf("entry %d: no %s videoId", i, ytNS)
		}
		if isShort(e.Links) {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

// isShort reports whether any of an entry's links names the Shorts player. A Short is a video like
// any other to the API — same id space, same contentDetails — and the only thing that separates it
// here is the URL it is presented under.
func isShort(links []atomLink) bool {
	for _, l := range links {
		if strings.Contains(l.Href, "/shorts/") {
			return true
		}
	}
	return false
}

// URL is the feed address for a channel.
func URL(channelID string) string {
	return "https://www.youtube.com/feeds/videos.xml?channel_id=" + url.QueryEscape(channelID)
}

// Client fetches feeds over HTTP.
type Client struct {
	HTTP *http.Client
}

// VideoIDs fetches one channel's feed and returns its video ids. The second return value is the
// number of billed API calls the lookup cost, which for the public feed is always zero — it is
// reported so the two upload sources satisfy the same contract.
func (c *Client) VideoIDs(ctx context.Context, channelID string) ([]string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, URL(channelID), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("channel %s: %w", channelID, err)
	}

	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("channel %s: %w", channelID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("channel %s: feed returned %s", channelID, resp.Status)
	}
	ids, err := Parse(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("channel %s: %w", channelID, err)
	}
	return ids, 0, nil
}
