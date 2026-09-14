// Command filler reconciles a YouTube playlist against a fixed set of channels, once, and exits.
//
// It is meant to be a CronJob: one shot, no daemon, no state carried between runs. Most runs find
// nothing to do and say so. The playlist is the only record of what has already been added, which
// is why the run always reads it before it writes anything — playlistItems.insert will happily add
// the same video twice.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"

	"github.com/sanchpet/youtube-playlist-filler/internal/config"
	"github.com/sanchpet/youtube-playlist-filler/internal/reconcile"
	"github.com/sanchpet/youtube-playlist-filler/internal/ytapi"
)

// dailyQuota is the API's budget, logged beside the run's estimate so the number is readable
// without knowing it.
const dailyQuota = 10000

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch err := run(ctx, os.Args[1:], log); {
	case err == nil:
	case errors.Is(err, flag.ErrHelp):
		// -h already printed the usage to stderr; a help request is not a failed run.
	default:
		log.Error("run failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, log *slog.Logger) error {
	cfg, err := config.Load(args, os.Stderr)
	if err != nil {
		return err
	}

	svc, err := youtube.NewService(ctx, option.WithTokenSource(tokenSource(ctx, cfg)))
	if err != nil {
		return fmt.Errorf("youtube service: %w", err)
	}

	targets := make([]reconcile.Options, 0, len(cfg.Targets))
	for _, t := range cfg.Targets {
		log.Info("target configured",
			"target", t.Name, "playlist", t.PlaylistID, "channels", len(t.Channels),
			"band", fmt.Sprintf("[%s, %s]", t.MinDuration, t.MaxDuration))
		targets = append(targets, reconcile.Options{
			Name:          t.Name,
			PlaylistID:    t.PlaylistID,
			Channels:      t.Channels,
			Min:           t.MinDuration,
			Max:           t.MaxDuration,
			DryRun:        cfg.DryRun,
			FullReconcile: cfg.FullReconcile,
		})
	}
	log.Info("run starting", "targets", len(targets),
		"max_inserts", cfg.MaxInserts, "dry_run", cfg.DryRun, "full_reconcile", cfg.FullReconcile)

	results, err := reconcile.RunAll(ctx, ytapi.New(svc, log), targets, cfg.MaxInserts, log)

	// The summaries are logged whether or not the run failed: a partial run has still spent quota
	// and still changed the playlist, and that is exactly when knowing how much matters.
	var inserted, units int
	for i, res := range results {
		log.Info("target finished", "target", targets[i].Name,
			"playlist_size", res.PlaylistSize, "candidates", res.Candidates, "in_band", res.InBand,
			"inserted", res.Inserted, "deferred", res.Deferred, "playlist_full", res.PlaylistFull,
			"dry_run", res.DryRun, "estimated_units", res.Units)
		inserted += res.Inserted
		units += res.Units
	}
	log.Info("run finished", "targets", len(results), "inserted", inserted,
		"dry_run", cfg.DryRun, "estimated_units", units, "daily_quota", dailyQuota)

	return err
}

// tokenSource mints access tokens from the stored refresh token. There is no interactive flow
// here on purpose — the job runs unattended, and the one consent this account ever gave was
// recorded once by cmd/enroll.
func tokenSource(ctx context.Context, cfg *config.Config) oauth2.TokenSource {
	oc := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{youtube.YoutubeScope},
	}
	return oc.TokenSource(ctx, &oauth2.Token{RefreshToken: cfg.RefreshToken})
}
