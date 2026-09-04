package ytapi_test

import (
	"testing"
	"time"

	"github.com/sanchpet/youtube-playlist-filler/internal/ytapi"
)

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"PT15M", 15 * time.Minute},
		{"PT1H", time.Hour},
		{"PT3H", 3 * time.Hour},
		{"PT1H23M45S", time.Hour + 23*time.Minute + 45*time.Second},
		{"PT11S", 11 * time.Second}, // the Short that a ceiling-only filter would admit
		{"PT0S", 0},                 // what a live or processing video reports
		{"P0D", 0},                  // and the other way the API says the same thing
		{"P1DT2H", 26 * time.Hour},  // past 24 hours the API switches to days
		{"P1DT2H3M4S", 26*time.Hour + 3*time.Minute + 4*time.Second},
		{"P2D", 48 * time.Hour},
		{"PT59M59S", 59*time.Minute + 59*time.Second},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ytapi.ParseDuration(c.in)
			if err != nil {
				t.Fatalf("ParseDuration(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("ParseDuration(%q) = %s, want %s", c.in, got, c.want)
			}
		})
	}
}

func TestParseDurationRejects(t *testing.T) {
	// "P5M" is the one that matters: before the T, M is months. Reading it as five minutes would
	// admit a video the band is meant to reject, and reading it as five months would reject one it
	// is meant to admit — so it is refused rather than guessed at.
	cases := []string{
		"", "15M", "T15M", "P", "PT", "PT15", "15", "PTM", "PT-5M",
		"P5M", "P1Y", "P2W", "PT1H2X", "PT1H1H", "PTT1H", "PT1M2H3M",
		"pt15m", "PT 15M", "PT1H30M15",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if got, err := ytapi.ParseDuration(in); err == nil {
				t.Errorf("ParseDuration(%q) = %s, want an error", in, got)
			}
		})
	}
}
