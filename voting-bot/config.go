package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	minAllowedVote = 1
	maxAllowedVote = 100
)

type Config struct {
	BotUserID         string
	OAuthToken        string
	ClientID          string
	BroadcasterUserID string

	BridgePort   string
	BridgeSecret string

	MinVote  int
	MaxVote  int
	LogLevel string
}

func loadConfig() (*Config, error) {
	cfg := &Config{
		BotUserID:         os.Getenv("BOT_USER_ID"),
		OAuthToken:        os.Getenv("OAUTH_TOKEN"),
		ClientID:          os.Getenv("CLIENT_ID"),
		BroadcasterUserID: os.Getenv("BROADCASTER_USER_ID"),
		BridgePort:        envOrDefault("BRIDGE_PORT", "3000"),
		BridgeSecret:      os.Getenv("BRIDGE_SECRET"),
		LogLevel:          envOrDefault("LOG_LEVEL", "info"),
	}

	missing := make([]string, 0)
	for _, pair := range [][2]string{
		{"BOT_USER_ID", cfg.BotUserID},
		{"OAUTH_TOKEN", cfg.OAuthToken},
		{"CLIENT_ID", cfg.ClientID},
		{"BROADCASTER_USER_ID", cfg.BroadcasterUserID},
	} {
		if pair[1] == "" {
			missing = append(missing, pair[0])
		}
	}
	if len(missing) > 0 {
		return nil, errors.New("missing required env vars: " + strings.Join(missing, ", "))
	}

	port, err := strconv.Atoi(cfg.BridgePort)
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("BRIDGE_PORT must be a valid port number (1-65535)")
	}

	var voteErr error
	if cfg.MinVote, voteErr = strconv.Atoi(envOrDefault("MIN_VOTE", "1")); voteErr != nil {
		return nil, errors.New("MIN_VOTE must be an integer")
	}
	if cfg.MaxVote, voteErr = strconv.Atoi(envOrDefault("MAX_VOTE", "5")); voteErr != nil {
		return nil, errors.New("MAX_VOTE must be an integer")
	}
	if cfg.MinVote < minAllowedVote || cfg.MaxVote > maxAllowedVote {
		return nil, errors.New("vote range must be within 1-100")
	}
	if cfg.MinVote >= cfg.MaxVote {
		return nil, errors.New("MIN_VOTE must be less than MAX_VOTE")
	}

	return cfg, nil
}

func (c *Config) NewLogger() *logrus.Logger {
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: time.RFC3339,
	})

	level, err := logrus.ParseLevel(c.LogLevel)
	if err != nil {
		// Fall back to info and let the caller surface this if needed.
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)

	return logger
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
