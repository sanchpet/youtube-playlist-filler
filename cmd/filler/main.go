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
	"github.com/sanchpet/youtube-playlist-filler/internal/feed"
	"github.com/sanchpet/youtube-playlist-filler/internal/reconcile"
	"github.com/sanchpet/youtube-playlist-filler/internal/ytapi"
)

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
	client := ytapi.New(svc, log)

	// The two upload sources differ only in what they cost and how far back they see. The feed is
	// free and stops at 15 entries; the uploads playlist is complete and charges a unit per fifty
	// videos, which is why it is a weekly schedule rather than the hourly one.
	var src reconcile.Source = &feed.Client{}
	if cfg.FullReconcile {
		src = ytapi.PlaylistSource{Client: client}
	}

	log.Info("run starting",
		"playlist", cfg.PlaylistID, "channels", len(cfg.Channels),
		"band", fmt.Sprintf("[%s, %s]", cfg.MinDuration, cfg.MaxDuration),
		"max_inserts", cfg.MaxInserts, "dry_run", cfg.DryRun, "full_reconcile", cfg.FullReconcile)

	res, err := reconcile.Run(ctx, client, src, reconcile.Options{
		PlaylistID: cfg.PlaylistID,
		Channels:   cfg.Channels,
		Min:        cfg.MinDuration,
		Max:        cfg.MaxDuration,
		MaxInserts: cfg.MaxInserts,
		DryRun:     cfg.DryRun,
	}, log)

	// The summary is logged whether or not the run failed: a partial run has still spent quota and
	// still changed the playlist, and that is exactly when knowing how much matters.
	log.Info("run finished",
		"playlist_size", res.PlaylistSize, "candidates", res.Candidates, "in_band", res.InBand,
		"inserted", res.Inserted, "deferred", res.Deferred, "playlist_full", res.PlaylistFull,
		"dry_run", res.DryRun, "estimated_units", res.Units, "daily_quota", 10000)

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
