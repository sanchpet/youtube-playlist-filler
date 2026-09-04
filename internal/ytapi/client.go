// Package ytapi is the YouTube Data API v3 surface this service uses: listing a playlist, reading
// video durations, and adding an item to a playlist.
//
// Three things about that API shape everything here.
//
// Cost. Reads are one unit each, playlistItems.insert is fifty, and the day's budget is ten
// thousand — so a run's spending is dominated entirely by how many videos it adds, and the reads
// that prevent a wrong add are effectively free. search.list is never called: it costs a hundred
// units and comes out of a separate daily allowance.
//
// Idempotence. There is none. Inserting a video that is already in the playlist succeeds and
// creates a second item pointing at the same video, so the playlist has to be read before it is
// written and every guard against duplicates is this service's own.
//
// Failure. Three unrelated conditions — a full playlist, an exhausted quota and a rate limit —
// all arrive as HTTP 403 and are told apart only by a reason string in the body.
package ytapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"google.golang.org/api/youtube/v3"
)

// Quota costs, in units, of the endpoints this package calls.
const (
	// CostList is playlistItems.list and videos.list, per call.
	CostList = 1
	// CostInsert is playlistItems.insert, per call — fifty times a read, which is what makes the
	// insert cap a quota control and the reads not worth economising on.
	CostInsert = 50
)

// AllPages asks PlaylistVideoIDs to follow every nextPageToken. Any other value bounds the read
// to that many pages.
const AllPages = 0

// pageSize is the API's own maximum for a list page. Fewer would cost the same per page and buy
// more pages.
const pageSize = 50

// BatchSize is the number of video ids videos.list accepts in one call. Sending them one at a
// time would still work and would cost fifty times as much.
const BatchSize = 50

// Client is the API surface the reconciler is given, with retries around it.
type Client struct {
	svc *youtube.Service
	log *slog.Logger

	// attempts bounds a single call including the first try; delay is the first backoff step,
	// doubled each time. Both are fields rather than constants so the retry loop is testable
	// without waiting on it.
	attempts int
	delay    time.Duration
	sleep    func(context.Context, time.Duration) error
}

// New builds a client over an authenticated YouTube service.
func New(svc *youtube.Service, log *slog.Logger) *Client {
	return &Client{
		svc:      svc,
		log:      log,
		attempts: 5,
		delay:    time.Second,
		sleep:    sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// do runs one API call under the retry policy: exponential backoff on a 5xx or a rate limit,
// immediate surrender on anything else. Jittered because every channel in a run hits the same
// endpoint, and an unjittered backoff would synchronise their retries into the same spike that
// caused the rate limit.
func (c *Client) do(ctx context.Context, op string, call func() error) error {
	delay := c.delay
	for attempt := 1; ; attempt++ {
		err := call()
		retryable, mapped := classify(err)
		if mapped == nil {
			return nil
		}
		if !retryable || attempt >= c.attempts {
			return fmt.Errorf("%s: %w", op, mapped)
		}

		wait := delay + rand.N(delay/2+1)
		c.log.Warn("retrying", "op", op, "attempt", attempt, "wait", wait, "err", err)
		if serr := c.sleep(ctx, wait); serr != nil {
			return fmt.Errorf("%s: %w", op, errors.Join(mapped, serr))
		}
		delay *= 2
	}
}

// PlaylistVideoIDs reads a playlist newest first and returns the video ids in it, with the number
// of billed calls that cost. maxPages bounds the read; AllPages follows every nextPageToken.
//
// It serves both halves of a run, and the bound is the only thing separating them. The target
// playlist is always read with AllPages, because a short read of it is indistinguishable from a
// playlist that is genuinely missing those videos and would have them added again. A channel's
// uploads playlist is read one page deep on the normal schedule — fifty newest is far more than a
// day's publishing — and with AllPages on the weekly pass.
//
// A page that fails aborts the whole read rather than yielding what it has, for the same reason:
// a partial answer here is a wrong answer that looks like a valid one.
func (c *Client) PlaylistVideoIDs(ctx context.Context, playlistID string, maxPages int) ([]string, int, error) {
	var (
		ids   []string
		calls int
		page  string
	)
	for {
		var resp *youtube.PlaylistItemListResponse
		err := c.do(ctx, "playlistItems.list", func() error {
			var err error
			resp, err = c.svc.PlaylistItems.List([]string{"contentDetails"}).
				PlaylistId(playlistID).
				MaxResults(pageSize).
				PageToken(page).
				Context(ctx).
				Do()
			return err
		})
		calls++
		if err != nil {
			return nil, calls, fmt.Errorf("playlist %s: %w", playlistID, err)
		}

		for _, item := range resp.Items {
			if item.ContentDetails == nil || item.ContentDetails.VideoId == "" {
				continue
			}
			ids = append(ids, item.ContentDetails.VideoId)
		}
		if resp.NextPageToken == "" || (maxPages != AllPages && calls >= maxPages) {
			return ids, calls, nil
		}
		page = resp.NextPageToken
	}
}

// UploadsPlaylistID is the auto-generated playlist holding every video a channel has uploaded.
// Its id is the channel id with the UC prefix replaced by UU — an undocumented but long-standing
// property of the id scheme, and the only way to enumerate a channel without paying for
// search.list.
func UploadsPlaylistID(channelID string) (string, error) {
	rest, ok := strings.CutPrefix(channelID, "UC")
	if !ok {
		return "", fmt.Errorf("channel id %q does not start with UC", channelID)
	}
	return "UU" + rest, nil
}

// Durations reads contentDetails for up to BatchSize video ids and returns the duration of each.
//
// Ids that come back missing are omitted rather than defaulted: a video can be deleted, private or
// region-blocked between being listed and this call, and giving it a duration of zero would let a
// band with no lower bound treat it as a candidate.
func (c *Client) Durations(ctx context.Context, ids []string) (map[string]time.Duration, int, error) {
	if len(ids) == 0 {
		return map[string]time.Duration{}, 0, nil
	}
	if len(ids) > BatchSize {
		return nil, 0, fmt.Errorf("videos.list: %d ids exceeds the batch limit of %d", len(ids), BatchSize)
	}

	var resp *youtube.VideoListResponse
	// One comma-joined value rather than the variadic form, which the client would send as
	// repeated id parameters.
	err := c.do(ctx, "videos.list", func() error {
		var err error
		resp, err = c.svc.Videos.List([]string{"contentDetails"}).
			Id(strings.Join(ids, ",")).
			Context(ctx).
			Do()
		return err
	})
	if err != nil {
		return nil, 1, err
	}

	out := make(map[string]time.Duration, len(resp.Items))
	for _, v := range resp.Items {
		if v.ContentDetails == nil {
			continue
		}
		d, err := ParseDuration(v.ContentDetails.Duration)
		if err != nil {
			// One unreadable duration is not a reason to drop the batch, but it is a reason to
			// say so: the video is left out, and the next run will consider it again.
			c.log.Warn("unparseable duration", "video", v.Id, "duration", v.ContentDetails.Duration, "err", err)
			continue
		}
		out[v.Id] = d
	}
	return out, 1, nil
}

// Insert adds one video to the playlist.
//
// snippet.position is deliberately not set. Setting it declares the playlist manually ordered, and
// YouTube then refuses every subsequent insert that does not carry a position with
// manualSortRequired — turning one cosmetic choice into a permanently broken reconciler.
func (c *Client) Insert(ctx context.Context, playlistID, videoID string) error {
	item := &youtube.PlaylistItem{
		Snippet: &youtube.PlaylistItemSnippet{
			PlaylistId: playlistID,
			ResourceId: &youtube.ResourceId{
				Kind:    "youtube#video",
				VideoId: videoID,
			},
		},
	}
	return c.do(ctx, "playlistItems.insert", func() error {
		_, err := c.svc.PlaylistItems.Insert([]string{"snippet"}, item).Context(ctx).Do()
		return err
	})
}
