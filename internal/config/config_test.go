package config_test

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sanchpet/youtube-playlist-filler/internal/config"
)

func withCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("YT_CLIENT_ID", "id")
	t.Setenv("YT_CLIENT_SECRET", "secret")
	t.Setenv("YT_REFRESH_TOKEN", "refresh")
}

func TestLoadDefaults(t *testing.T) {
	withCredentials(t)

	cfg, err := config.Load(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.PlaylistID != config.DefaultPlaylistID {
		t.Errorf("playlist = %q, want %q", cfg.PlaylistID, config.DefaultPlaylistID)
	}
	if len(cfg.Channels) != len(config.DefaultChannels) {
		t.Errorf("got %d channels, want %d", len(cfg.Channels), len(config.DefaultChannels))
	}
	if cfg.MinDuration != 15*time.Minute || cfg.MaxDuration != 3*time.Hour {
		t.Errorf("band = [%s, %s], want [15m, 3h]", cfg.MinDuration, cfg.MaxDuration)
	}
	if cfg.DryRun || cfg.FullReconcile {
		t.Error("neither dry-run nor full-reconcile should be on by default")
	}
}

func TestFlagsBeatEnvironment(t *testing.T) {
	withCredentials(t)
	t.Setenv("YT_MIN_DURATION", "5m")

	cfg, err := config.Load([]string{"-min-duration", "20m", "-dry-run", "-full-reconcile"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.MinDuration != 20*time.Minute {
		t.Errorf("min = %s, want 20m", cfg.MinDuration)
	}
	if !cfg.DryRun || !cfg.FullReconcile {
		t.Error("flags did not take")
	}
}

func TestEnvironmentConfiguresTheBandAndTheChannels(t *testing.T) {
	withCredentials(t)
	t.Setenv("YT_MIN_DURATION", "1m")
	t.Setenv("YT_MAX_DURATION", "10m")
	t.Setenv("YT_MAX_INSERTS", "7")
	t.Setenv("YT_CHANNEL_IDS", "UCaaa, UCbbb ,,UCccc")
	t.Setenv("YT_PLAYLIST_ID", "PLother")

	cfg, err := config.Load(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.MinDuration != time.Minute || cfg.MaxDuration != 10*time.Minute || cfg.MaxInserts != 7 {
		t.Errorf("got [%s, %s] cap %d", cfg.MinDuration, cfg.MaxDuration, cfg.MaxInserts)
	}
	if got := strings.Join(cfg.Channels, "|"); got != "UCaaa|UCbbb|UCccc" {
		t.Errorf("channels = %q", got)
	}
	if cfg.PlaylistID != "PLother" {
		t.Errorf("playlist = %q", cfg.PlaylistID)
	}
}

// An inverted band matches nothing, so the run succeeds, reports no candidates and looks exactly
// like a quiet week. It has to be refused at startup.
func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		args []string
	}{
		{"inverted band", nil, []string{"-min-duration", "3h", "-max-duration", "15m"}},
		{"zero ceiling", nil, []string{"-max-duration", "0"}},
		{"negative floor", nil, []string{"-min-duration", "-5m"}},
		{"no inserts allowed", nil, []string{"-max-inserts", "0"}},
		{"no channels", map[string]string{"YT_CHANNEL_IDS": " , "}, nil},
		{"no playlist", map[string]string{"YT_PLAYLIST_ID": " "}, []string{"-playlist", ""}},
		{"unparseable cap", map[string]string{"YT_MAX_INSERTS": "1O0"}, nil},
		{"unparseable band", map[string]string{"YT_MIN_DURATION": "15 minutes"}, nil},
		{"unparseable flag", map[string]string{"YT_DRY_RUN": "maybe"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withCredentials(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			if cfg, err := config.Load(c.args, io.Discard); err == nil {
				t.Errorf("want an error, got %+v", cfg)
			}
		})
	}
}

func TestLoadRequiresCredentials(t *testing.T) {
	for _, missing := range []string{"YT_CLIENT_ID", "YT_CLIENT_SECRET", "YT_REFRESH_TOKEN"} {
		t.Run(missing, func(t *testing.T) {
			withCredentials(t)
			t.Setenv(missing, "")
			if _, err := config.Load(nil, io.Discard); err == nil {
				t.Errorf("want an error when %s is unset", missing)
			}
		})
	}
}
