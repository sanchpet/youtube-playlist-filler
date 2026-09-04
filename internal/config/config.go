// Package config reads the run's settings from flags and the environment.
//
// Which playlist and which channels are configuration, not code: they are the one part of this
// service that changes without the behaviour changing, and a channel added by editing a Go file is
// a release. The defaults below are what the deployment currently runs, so the binary is useful
// with an empty environment; every one of them can be replaced without a rebuild.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultPlaylistID is the playlist the service maintains.
const DefaultPlaylistID = "PLbkjH9B2w9JzHthzAcmAnJ4N7ymUQOyOC"

// DefaultChannels is the channel set. The trailing comment is the channel's name at the time of
// writing, which is the only part of this that can change without anyone noticing — a channel gets
// renamed, its id does not.
var DefaultChannels = []string{
	"UCoH2qJSyODQpBKsK63Moc6Q", // spiritual brother Sci-Fi
	"UCArcuj2cwzXsCZVcdsAEIKQ", // Everlasting Ripples
	"UCmoxjjB1ZY2W5mSbz0TpmPA", // spiritual brother
	"UCDyghjHud0sexPHxs5qPXUQ", // Futurescapes Sci-Fi
	"UC7GZzOWjuG4wMua82uEguXQ", // OBSIDIAN SOUNDFIELDS
	"UCGLsjcH6HrK2pr2x7mCA25w", // Quiet Dystopia
	"UC2e1MRnt4IT5KEUh_-iP-Ug", // Future City Music
	"UCdxcdfiLlxIHqto0fw5FNWw", // Lost Sounds
	"UCKWOEUGWIsVYK9Yz4hVjbCg", // Focus Soundscapes
	"UCj3460Ylt4JEcEiW6PxCH9Q", // SpaceWave
}

// Duration band defaults. The floor is what keeps Shorts, teasers and trailers out; the ceiling
// keeps a single eight-hour upload from standing in for a whole listening session.
const (
	DefaultMinDuration = 15 * time.Minute
	DefaultMaxDuration = 3 * time.Hour
)

// DefaultMaxInserts is the quota fuse. At fifty units an insert against a ten-thousand-unit day,
// a hundred inserts is half the budget — enough for any day on which ten channels genuinely
// published, and far short of the ~196 that would spend the lot.
const DefaultMaxInserts = 100

// Config is one run's settings.
type Config struct {
	PlaylistID string
	Channels   []string

	ClientID     string
	ClientSecret string
	RefreshToken string

	MinDuration time.Duration
	MaxDuration time.Duration
	MaxInserts  int

	DryRun bool
	// FullReconcile walks every page of each channel's uploads playlist instead of only the
	// newest one. A normal run reads a bounded window and has no way of telling what fell out the
	// back of it, so this is the pass that recovers a burst of publishing.
	FullReconcile bool
}

// Load parses flags over environment defaults, so a CronJob is configured entirely through its env
// and a person can still override any of it on the command line for a single run.
func Load(args []string, out io.Writer) (*Config, error) {
	c := &Config{
		ClientID:     os.Getenv("YT_CLIENT_ID"),
		ClientSecret: os.Getenv("YT_CLIENT_SECRET"),
		RefreshToken: os.Getenv("YT_REFRESH_TOKEN"),
	}

	// Environment defaults are resolved before the flags are declared, and a malformed one is an
	// error rather than a fallback to the default: YT_MAX_INSERTS=1O0 silently meaning 100 is the
	// kind of thing found months later, in a quota graph.
	var envErrs []error
	envD := func(key string, def time.Duration) time.Duration {
		v, ok := os.LookupEnv(key)
		if !ok || v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			envErrs = append(envErrs, fmt.Errorf("%s: %w", key, err))
			return def
		}
		return d
	}
	envI := func(key string, def int) int {
		v, ok := os.LookupEnv(key)
		if !ok || v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			envErrs = append(envErrs, fmt.Errorf("%s: %w", key, err))
			return def
		}
		return n
	}
	envB := func(key string) bool {
		v, ok := os.LookupEnv(key)
		if !ok || v == "" {
			return false
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			envErrs = append(envErrs, fmt.Errorf("%s: %w", key, err))
			return false
		}
		return b
	}

	fs := flag.NewFlagSet("youtube-playlist-filler", flag.ContinueOnError)
	fs.SetOutput(out)

	channels := fs.String("channels", envOr("YT_CHANNEL_IDS", strings.Join(DefaultChannels, ",")),
		"comma-separated channel ids to reconcile from")
	fs.StringVar(&c.PlaylistID, "playlist", envOr("YT_PLAYLIST_ID", DefaultPlaylistID),
		"target playlist id")
	fs.DurationVar(&c.MinDuration, "min-duration", envD("YT_MIN_DURATION", DefaultMinDuration),
		"shortest video to add, inclusive")
	fs.DurationVar(&c.MaxDuration, "max-duration", envD("YT_MAX_DURATION", DefaultMaxDuration),
		"longest video to add, inclusive")
	fs.IntVar(&c.MaxInserts, "max-inserts", envI("YT_MAX_INSERTS", DefaultMaxInserts),
		"quota fuse: most videos one run will add")
	fs.BoolVar(&c.DryRun, "dry-run", envB("YT_DRY_RUN"),
		"do everything except the inserts, and log what would be added")
	fs.BoolVar(&c.FullReconcile, "full-reconcile", envB("YT_FULL_RECONCILE"),
		"walk every page of each channel's uploads playlist, not just the newest: costs quota, recovers what a bounded window missed")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if len(envErrs) > 0 {
		return nil, errors.Join(envErrs...)
	}

	c.Channels = splitList(*channels)
	return c, c.validate()
}

// validate refuses a configuration that would run and quietly do the wrong thing. The inverted
// band is the one worth naming: it matches nothing, so the run succeeds, reports no candidates,
// and is indistinguishable from a week in which no channel published.
func (c *Config) validate() error {
	switch {
	case c.PlaylistID == "":
		return errors.New("playlist id is required (YT_PLAYLIST_ID or -playlist)")
	case len(c.Channels) == 0:
		return errors.New("at least one channel is required (YT_CHANNEL_IDS or -channels)")
	case c.MinDuration < 0 || c.MaxDuration <= 0:
		return fmt.Errorf("duration band must be positive, got [%s, %s]", c.MinDuration, c.MaxDuration)
	case c.MinDuration > c.MaxDuration:
		return fmt.Errorf("duration band is inverted: min %s is longer than max %s", c.MinDuration, c.MaxDuration)
	case c.MaxInserts <= 0:
		return fmt.Errorf("max inserts must be positive, got %d", c.MaxInserts)
	}
	for name, v := range map[string]string{
		"YT_CLIENT_ID": c.ClientID, "YT_CLIENT_SECRET": c.ClientSecret, "YT_REFRESH_TOKEN": c.RefreshToken,
	} {
		if v == "" {
			return fmt.Errorf("%s is required (cmd/enroll mints the refresh token)", name)
		}
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
