package ytapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
)

func apiErr(code int, reason string) error {
	return &googleapi.Error{Code: code, Errors: []googleapi.ErrorItem{{Reason: reason}}}
}

// The three conditions that matter all arrive as 403 and differ only by the reason string. Getting
// this wrong is not a crash — it is a job that retries an exhausted quota until the backoff gives
// up and then reports it as a rate limit.
func TestClassify(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		sentinel  error
		retryable bool
	}{
		{"full playlist", apiErr(http.StatusForbidden, "playlistContainsMaximumNumberOfVideos"), ErrPlaylistFull, false},
		{"quota", apiErr(http.StatusForbidden, "quotaExceeded"), ErrQuotaExceeded, false},
		{"daily limit", apiErr(http.StatusForbidden, "dailyLimitExceeded"), ErrQuotaExceeded, false},
		{"rate limit", apiErr(http.StatusForbidden, "rateLimitExceeded"), nil, true},
		{"server error", apiErr(http.StatusInternalServerError, ""), nil, true},
		{"bad gateway", apiErr(http.StatusBadGateway, ""), nil, true},
		{"not found", apiErr(http.StatusNotFound, "playlistNotFound"), nil, false},
		{"forbidden, no reason", apiErr(http.StatusForbidden, ""), nil, false},
		{"transport", io.ErrUnexpectedEOF, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			retryable, mapped := classify(c.err)
			if retryable != c.retryable {
				t.Errorf("retryable = %v, want %v", retryable, c.retryable)
			}
			if c.sentinel != nil && !errors.Is(mapped, c.sentinel) {
				t.Errorf("got %v, want it to wrap %v", mapped, c.sentinel)
			}
			if c.sentinel == nil {
				for _, s := range []error{ErrPlaylistFull, ErrQuotaExceeded} {
					if errors.Is(mapped, s) {
						t.Errorf("got %v, which wrongly wraps %v", mapped, s)
					}
				}
			}
		})
	}
	if retryable, mapped := classify(nil); mapped != nil || retryable {
		t.Errorf("classify(nil) = %v, %v; want false, nil", retryable, mapped)
	}
}

func testClient(t *testing.T) (*Client, *int) {
	t.Helper()
	slept := 0
	return &Client{
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		attempts: 4,
		delay:    time.Nanosecond,
		sleep: func(context.Context, time.Duration) error {
			slept++
			return nil
		},
	}, &slept
}

func TestDoRetriesRateLimitAndGivesUp(t *testing.T) {
	c, slept := testClient(t)

	calls := 0
	err := c.do(t.Context(), "videos.list", func() error {
		calls++
		return apiErr(http.StatusForbidden, "rateLimitExceeded")
	})

	if err == nil {
		t.Fatal("want an error once the attempts are spent")
	}
	if calls != 4 {
		t.Errorf("made %d calls, want 4 (the configured attempts)", calls)
	}
	if *slept != 3 {
		t.Errorf("backed off %d times, want 3 (one fewer than the attempts)", *slept)
	}
}

func TestDoDoesNotRetryQuota(t *testing.T) {
	c, slept := testClient(t)

	calls := 0
	err := c.do(t.Context(), "playlistItems.insert", func() error {
		calls++
		return apiErr(http.StatusForbidden, "quotaExceeded")
	})

	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("got %v, want it to wrap ErrQuotaExceeded", err)
	}
	if calls != 1 {
		t.Errorf("made %d calls, want 1: the budget is spent until midnight Pacific and every "+
			"retry is another failure charged for", calls)
	}
	if *slept != 0 {
		t.Errorf("backed off %d times, want 0", *slept)
	}
}

func TestDoRecoversAfterServerError(t *testing.T) {
	c, _ := testClient(t)

	calls := 0
	err := c.do(t.Context(), "playlistItems.list", func() error {
		calls++
		if calls < 3 {
			return apiErr(http.StatusServiceUnavailable, "")
		}
		return nil
	})

	if err != nil {
		t.Fatalf("want the third attempt to succeed, got %v", err)
	}
	if calls != 3 {
		t.Errorf("made %d calls, want 3", calls)
	}
}

func TestUploadsPlaylistID(t *testing.T) {
	got, err := UploadsPlaylistID("UCoH2qJSyODQpBKsK63Moc6Q")
	if err != nil {
		t.Fatal(err)
	}
	if want := "UUoH2qJSyODQpBKsK63Moc6Q"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if _, err := UploadsPlaylistID("PLnotachannel"); err == nil {
		t.Error("want an error for an id that is not a channel")
	}
}
