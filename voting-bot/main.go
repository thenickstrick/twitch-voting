package main

import (
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
		bootstrap.WithError(err).Fatal("Failed to load configuration for dti-chatbot.")
	}

	logger := cfg.NewLogger()

	votes := newVoteState(cfg.MinVote, cfg.MaxVote)

	client := newTwitchClient(cfg, votes, logger)
	if err := client.validateAuth(); err != nil {
		logger.WithError(err).Fatal("Failed to validate Twitch auth token for dti-chatbot.")
	}

	b := newBridge(cfg, votes, logger)
	go b.start()

	// Blocks forever, reconnecting on failure.
	client.run()
}
