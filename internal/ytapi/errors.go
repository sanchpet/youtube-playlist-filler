package ytapi

import (
	"errors"
	"fmt"

	"google.golang.org/api/googleapi"
)

// Terminal conditions the caller has to tell apart. All three arrive as HTTP 403 and differ only
// by the reason string inside the body, so classifying on the status code alone would retry a
// quota exhaustion for as long as the backoff allows and then report it as a rate limit.
var (
	// ErrPlaylistFull is the playlist having reached the number of videos YouTube allows in one.
	// The number is not documented and is not hard-coded here: the error is the contract.
	ErrPlaylistFull = errors.New("playlist has reached its maximum number of videos")

	// ErrQuotaExceeded is the day's units being spent. Never retried — the budget resets at
	// midnight Pacific, and every retry before then is another failure charged for.
	ErrQuotaExceeded = errors.New("youtube api quota exceeded")
)

// reasons returns the typed error codes on a googleapi error, which is where YouTube states what
// actually went wrong; the HTTP status alone does not distinguish them.
func reasons(err error) []string {
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) {
		return nil
	}
	out := make([]string, 0, len(gerr.Errors))
	for _, e := range gerr.Errors {
		out = append(out, e.Reason)
	}
	return out
}

func status(err error) int {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return gerr.Code
	}
	return 0
}

func hasReason(err error, want ...string) bool {
	for _, got := range reasons(err) {
		for _, w := range want {
			if got == w {
				return true
			}
		}
	}
	return false
}

// classify maps an API error onto what the caller should do with it: whether trying again could
// plausibly succeed, and the sentinel the error stands for.
func classify(err error) (retryable bool, mapped error) {
	switch {
	case err == nil:
		return false, nil
	case hasReason(err, "playlistContainsMaximumNumberOfVideos"):
		return false, fmt.Errorf("%w: %w", ErrPlaylistFull, err)
	case hasReason(err, "quotaExceeded", "dailyLimitExceeded"):
		return false, fmt.Errorf("%w: %w", ErrQuotaExceeded, err)
	case hasReason(err, "rateLimitExceeded", "userRateLimitExceeded"):
		// A rate limit is a request to slow down, not a refusal — the only 403 worth retrying.
		return true, err
	case status(err) >= 500:
		return true, err
	}
	return false, err
}
