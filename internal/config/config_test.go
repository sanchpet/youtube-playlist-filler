package config_test

import (
	"io"
	"os"
	"path/filepath"
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

func writeTargets(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "targets.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	withCredentials(t)

	cfg, err := config.Load(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(cfg.Targets))
	}
	tg := cfg.Targets[0]
	if tg.PlaylistID != config.DefaultPlaylistID {
		t.Errorf("playlist = %q, want %q", tg.PlaylistID, config.DefaultPlaylistID)
	}
	if len(tg.Channels) != len(config.DefaultChannels) {
		t.Errorf("got %d channels, want %d", len(tg.Channels), len(config.DefaultChannels))
	}
	if tg.MinDuration != 15*time.Minute || tg.MaxDuration != 3*time.Hour {
		t.Errorf("band = [%s, %s], want [15m, 3h]", tg.MinDuration, tg.MaxDuration)
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

	if got := cfg.Targets[0].MinDuration; got != 20*time.Minute {
		t.Errorf("min = %s, want 20m", got)
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

	tg := cfg.Targets[0]
	if tg.MinDuration != time.Minute || tg.MaxDuration != 10*time.Minute || cfg.MaxInserts != 7 {
		t.Errorf("got [%s, %s] cap %d", tg.MinDuration, tg.MaxDuration, cfg.MaxInserts)
	}
	if got := strings.Join(tg.Channels, "|"); got != "UCaaa|UCbbb|UCccc" {
		t.Errorf("channels = %q", got)
	}
	if tg.PlaylistID != "PLother" {
		t.Errorf("playlist = %q", tg.PlaylistID)
	}
}

func TestTargetsFileNamesEveryPlaylist(t *testing.T) {
	withCredentials(t)
	path := writeTargets(t, `
targets:
  - name: ambient
    playlist: PLambient
    channels:
      - UCaaa # a comment, as the channel's name
      - UCbbb
  - name: piano
    playlist: PLpiano
    min_duration: 20m
    max_duration: 4h
    channels: [UCccc]
`)

	cfg, err := config.Load([]string{"-targets", path, "-max-inserts", "40"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(cfg.Targets))
	}
	ambient, piano := cfg.Targets[0], cfg.Targets[1]
	if ambient.Name != "ambient" || ambient.PlaylistID != "PLambient" || strings.Join(ambient.Channels, "|") != "UCaaa|UCbbb" {
		t.Errorf("ambient = %+v", ambient)
	}
	if ambient.MinDuration != config.DefaultMinDuration || ambient.MaxDuration != config.DefaultMaxDuration {
		t.Errorf("an omitted band should take the defaults, got [%s, %s]", ambient.MinDuration, ambient.MaxDuration)
	}
	if piano.MinDuration != 20*time.Minute || piano.MaxDuration != 4*time.Hour {
		t.Errorf("piano band = [%s, %s], want [20m, 4h]", piano.MinDuration, piano.MaxDuration)
	}
	if cfg.MaxInserts != 40 {
		t.Errorf("max inserts = %d, want 40", cfg.MaxInserts)
	}
}

func TestTargetsFileFromEnvironment(t *testing.T) {
	withCredentials(t)
	t.Setenv("YT_TARGETS_FILE", writeTargets(t, "targets:\n  - {name: a, playlist: PLa, channels: [UCa]}\n"))

	cfg, err := config.Load(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Targets) != 1 || cfg.Targets[0].PlaylistID != "PLa" {
		t.Errorf("targets = %+v", cfg.Targets)
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

func TestTargetsFileRejects(t *testing.T) {
	const good = "targets:\n  - {name: a, playlist: PLa, channels: [UCa]}\n"
	cases := []struct {
		name string
		body string
		env  map[string]string
		args []string
	}{
		{"no targets", "targets: []\n", nil, nil},
		{"unknown key", "targets:\n  - {name: a, playlist: PLa, channels: [UCa], max_duraton: 1h}\n", nil, nil},
		{"unparseable band", "targets:\n  - {name: a, playlist: PLa, channels: [UCa], min_duration: 15 minutes}\n", nil, nil},
		{"inverted band", "targets:\n  - {name: a, playlist: PLa, channels: [UCa], min_duration: 3h, max_duration: 15m}\n", nil, nil},
		{"no name", "targets:\n  - {playlist: PLa, channels: [UCa]}\n", nil, nil},
		{"no playlist", "targets:\n  - {name: a, channels: [UCa]}\n", nil, nil},
		{"no channels", "targets:\n  - {name: a, playlist: PLa}\n", nil, nil},
		{"duplicate name", good + "  - {name: a, playlist: PLb, channels: [UCb]}\n", nil, nil},
		{"duplicate playlist", good + "  - {name: b, playlist: PLa, channels: [UCb]}\n", nil, nil},
		{"combined with a playlist variable", good, map[string]string{"YT_PLAYLIST_ID": "PLx"}, nil},
		{"combined with a band flag", good, nil, []string{"-min-duration", "5m"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withCredentials(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			args := append([]string{"-targets", writeTargets(t, c.body)}, c.args...)
			if cfg, err := config.Load(args, io.Discard); err == nil {
				t.Errorf("want an error, got %+v", cfg)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		withCredentials(t)
		if _, err := config.Load([]string{"-targets", filepath.Join(t.TempDir(), "absent.yaml")}, io.Discard); err == nil {
			t.Error("want an error for a targets file that does not exist")
		}
	})
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
