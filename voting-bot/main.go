package main

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/sirupsen/logrus"
)

func main() {
	// Load .env if present — ignore error, env vars may be set externally.
	_ = godotenv.Load()

	// Bootstrap logger for errors that occur before config is available.
	bootstrap := logrus.New()

	cfg, err := loadConfig()
	if err != nil {
		bootstrap.WithError(err).Fatal("Failed to load configuration for voting-bot.")
	}

	logger := cfg.NewLogger()

	votes := newVoteState(cfg.MinVote, cfg.MaxVote)

	// Top-level ctx cancels on SIGINT/SIGTERM and triggers graceful shutdown of
	// every goroutine below. Also cancelled manually when any component fails
	// fatally, so one broken piece drags the whole process down cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := newTwitchClient(cfg, votes, logger)
	if err := client.validate(ctx); err != nil {
		logger.WithError(err).Fatal("Failed to validate Twitch auth token for voting-bot.")
	}

	b := newBridge(cfg, votes, logger)

	// Buffer matches the number of goroutines sending so a failing component
	// never blocks on the channel send, even if we're already shutting down.
	errCh := make(chan error, 3)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := b.start(ctx); err != nil {
			errCh <- err
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := client.run(ctx); err != nil {
			errCh <- err
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		client.refreshLoop(ctx)
	}()

	// Block until either a fatal component error or an OS signal. Either way,
	// cancel ctx so remaining goroutines unwind, then wait for them.
	select {
	case err := <-errCh:
		logger.WithError(err).Error("Component failed, shutting down voting-bot.")
		stop()
	case <-ctx.Done():
		logger.Info("Shutdown signal received, stopping voting-bot.")
	}

	wg.Wait()
	logger.Info("voting-bot stopped.")
}
