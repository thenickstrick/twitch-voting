package main

import (
	"errors"
	"fmt"
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
	RefreshToken      string
	ClientID          string
	ClientSecret      string
	BroadcasterUserID string

	BridgePort         string
	BridgeSecret       string
	BridgeAuthDisabled bool

	MinVote  int
	MaxVote  int
	LogLevel string
}

func loadConfig() (*Config, error) {
	cfg := &Config{
		BotUserID:          os.Getenv("BOT_USER_ID"),
		OAuthToken:         os.Getenv("OAUTH_TOKEN"),
		RefreshToken:       os.Getenv("REFRESH_TOKEN"),
		ClientID:           os.Getenv("CLIENT_ID"),
		ClientSecret:       os.Getenv("CLIENT_SECRET"),
		BroadcasterUserID:  os.Getenv("BROADCASTER_USER_ID"),
		BridgePort:         envOrDefault("BRIDGE_PORT", "3000"),
		BridgeSecret:       os.Getenv("BRIDGE_SECRET"),
		BridgeAuthDisabled: os.Getenv("BRIDGE_AUTH_DISABLED") == "true",
		LogLevel:           envOrDefault("LOG_LEVEL", "info"),
	}

	missing := make([]string, 0)
	for _, pair := range [][2]string{
		{"BOT_USER_ID", cfg.BotUserID},
		{"OAUTH_TOKEN", cfg.OAuthToken},
		{"REFRESH_TOKEN", cfg.RefreshToken},
		{"CLIENT_ID", cfg.ClientID},
		{"CLIENT_SECRET", cfg.ClientSecret},
		{"BROADCASTER_USER_ID", cfg.BroadcasterUserID},
	} {
		if pair[1] == "" {
			missing = append(missing, pair[0])
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}

	// Fail closed: require BRIDGE_SECRET unless BRIDGE_AUTH_DISABLED is explicitly set.
	// Without this, an empty secret would leave the privileged bridge routes open to anyone
	// who can reach the bot — and Traefik publishes them on the configured domain.
	if !cfg.BridgeAuthDisabled && cfg.BridgeSecret == "" {
		return nil, errors.New("BRIDGE_SECRET is required (set BRIDGE_AUTH_DISABLED=true for localhost-only dev mode)")
	}

	port, err := strconv.Atoi(cfg.BridgePort)
	if err != nil {
		return nil, fmt.Errorf("BRIDGE_PORT must be an integer: %w", err)
	}
	if port < 1 || port > 65535 {
		return nil, errors.New("BRIDGE_PORT must be between 1 and 65535")
	}

	minVote, err := strconv.Atoi(envOrDefault("MIN_VOTE", "1"))
	if err != nil {
		return nil, fmt.Errorf("MIN_VOTE must be an integer: %w", err)
	}
	maxVote, err := strconv.Atoi(envOrDefault("MAX_VOTE", "5"))
	if err != nil {
		return nil, fmt.Errorf("MAX_VOTE must be an integer: %w", err)
	}
	cfg.MinVote = minVote
	cfg.MaxVote = maxVote

	if cfg.MinVote < minAllowedVote || cfg.MinVote > maxAllowedVote ||
		cfg.MaxVote < minAllowedVote || cfg.MaxVote > maxAllowedVote {
		return nil, fmt.Errorf("vote range must be within %d-%d", minAllowedVote, maxAllowedVote)
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
