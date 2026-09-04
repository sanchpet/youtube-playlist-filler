package ytapi

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseDuration reads the ISO 8601 duration YouTube reports in contentDetails.duration.
//
// Hand-parsed rather than pattern-matched because the one mistake that matters here is silent:
// `M` means months before the `T` and minutes after it, and a parser that ignores the separator
// turns a five-minute video into a five-month one — which passes any upper bound the band has.
// So the calendar-length units are refused outright instead of being guessed at; YouTube does not
// emit them for a video, and a duration that needs them is not something to reconcile blind.
//
// Accepted: PT#H#M#S in any combination, and the P#DT#H#M#S form the API uses for anything at or
// past 24 hours. Rejected: an empty duration, a missing P, a component with no unit or a unit with
// no number, a repeated unit, and the Y/M/W units.
func ParseDuration(s string) (time.Duration, error) {
	rest, ok := strings.CutPrefix(s, "P")
	if !ok {
		return 0, fmt.Errorf("duration %q: missing leading P", s)
	}

	var (
		total  time.Duration
		num    strings.Builder
		inTime bool
		seen   = map[byte]bool{}
	)
	for i := range len(rest) {
		c := rest[i]
		if c >= '0' && c <= '9' {
			num.WriteByte(c)
			continue
		}
		if c == 'T' {
			if inTime {
				return 0, fmt.Errorf("duration %q: repeated T", s)
			}
			if num.Len() > 0 {
				return 0, fmt.Errorf("duration %q: %s has no unit", s, num.String())
			}
			inTime = true
			continue
		}

		unit, err := unitOf(c, inTime)
		if err != nil {
			return 0, fmt.Errorf("duration %q: %w", s, err)
		}
		if num.Len() == 0 {
			return 0, fmt.Errorf("duration %q: %c has no number", s, c)
		}
		if seen[c] {
			return 0, fmt.Errorf("duration %q: repeated %c", s, c)
		}
		seen[c] = true

		n, err := strconv.ParseInt(num.String(), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("duration %q: %w", s, err)
		}
		total += time.Duration(n) * unit
		num.Reset()
	}

	if num.Len() > 0 {
		return 0, fmt.Errorf("duration %q: %s has no unit", s, num.String())
	}
	if len(seen) == 0 {
		return 0, fmt.Errorf("duration %q: no components", s)
	}
	return total, nil
}

// unitOf resolves a unit letter against the side of the T separator it appeared on.
func unitOf(c byte, afterT bool) (time.Duration, error) {
	if afterT {
		switch c {
		case 'H':
			return time.Hour, nil
		case 'M':
			return time.Minute, nil
		case 'S':
			return time.Second, nil
		}
		return 0, fmt.Errorf("unknown time unit %c", c)
	}
	switch c {
	case 'D':
		return 24 * time.Hour, nil
	case 'Y', 'M', 'W':
		return 0, fmt.Errorf("calendar unit %c is not a fixed length", c)
	}
	return 0, fmt.Errorf("unknown date unit %c", c)
}
