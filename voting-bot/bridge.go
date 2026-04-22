package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

const (
	serverReadHeaderTimeout = 2 * time.Second
	serverReadTimeout       = 5 * time.Second
	serverWriteTimeout      = 10 * time.Second
	serverIdleTimeout       = 60 * time.Second
	maxRequestBodyBytes     = 1024

	// Only the unauthenticated GET /api/votes path is rate-limited — authenticated
	// routes are already gated by the bridge secret. Global limiter: good enough
	// for the expected single-consumer polling pattern. Can be upgraded to per-IP
	// if the deployment ever hosts multiple external consumers.
	votesReadRatePerSecond = 30
	votesReadBurst         = 60

	bridgeShutdownTimeout = 5 * time.Second
)

type bridge struct {
	cfg          *Config
	votes        *VoteState
	logger       *logrus.Logger
	votesLimiter *rate.Limiter
}

func newBridge(cfg *Config, votes *VoteState, logger *logrus.Logger) *bridge {
	return &bridge{
		cfg:          cfg,
		votes:        votes,
		logger:       logger,
		votesLimiter: rate.NewLimiter(rate.Limit(votesReadRatePerSecond), votesReadBurst),
	}
}

// start runs the HTTP bridge until ctx is cancelled, then triggers a graceful
// shutdown. Returns a non-nil error if the server fails to start or shut down
// cleanly; main will log and exit.
func (b *bridge) start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/votes", b.rateLimited(b.handleVotes))
	mux.HandleFunc("/api/votes/detail", b.requireSecret(b.handleDetail))
	mux.HandleFunc("/api/votes/start", b.requireSecret(b.handleStart))
	mux.HandleFunc("/api/votes/cast", b.requireSecret(b.handleCast))
	mux.HandleFunc("/api/votes/end", b.requireSecret(b.handleEnd))
	mux.HandleFunc("/api/votes/reset", b.requireSecret(b.handleReset))

	// When bridge auth is disabled we bind to loopback only. Otherwise unauthenticated
	// privileged routes would be exposed to anything that can reach the bot's port
	// (including Traefik, which publishes it on the configured domain).
	addr := ":" + b.cfg.BridgePort
	if b.cfg.BridgeAuthDisabled {
		addr = "127.0.0.1:" + b.cfg.BridgePort
		b.logger.Warn("Bridge auth disabled via BRIDGE_AUTH_DISABLED, binding to localhost only.")
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		ReadTimeout:       serverReadTimeout,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		b.logger.WithField("addr", addr).Info("HTTP bridge listening.")
		// ListenAndServe only returns a non-http.ErrServerClosed error when the
		// listener itself fails (port in use, bind error) — that is a real failure
		// we want main to learn about.
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), bridgeShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("bridge graceful shutdown failed: %w", err)
		}
		b.logger.Info("HTTP bridge stopped.")
		return <-serveErr
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("bridge server failed: %w", err)
		}
		return nil
	}
}

// GET /api/votes
func (b *bridge) handleVotes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method not allowed"))
		return
	}
	writeJSON(w, http.StatusOK, b.votes.GetTally())
}

// GET /api/votes/detail — requires auth; shows per-viewer votes
func (b *bridge) handleDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method not allowed"))
		return
	}
	writeJSON(w, http.StatusOK, b.votes.GetDetailedVotes())
}

// POST /api/votes/start  body: {"players":["player1","player2"]}
func (b *bridge) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method not allowed"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)

	var body struct {
		Players []string `json:"players"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid request body"))
		return
	}
	if len(body.Players) == 0 {
		writeJSON(w, http.StatusBadRequest, errorBody("at least one player is required"))
		return
	}
	if len(body.Players) > maxPlayers {
		writeJSON(w, http.StatusBadRequest, errorBody(fmt.Sprintf("too many players (max %d)", maxPlayers)))
		return
	}
	if err := validatePlayerNames(body.Players); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	b.votes.OpenVoting(body.Players)
	b.logger.WithField("players", body.Players).Info("Voting opened via HTTP bridge.")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "players": body.Players})
}

// POST /api/votes/cast  body: {"player":"Player1","username":"viewer1","value":4}
func (b *bridge) handleCast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method not allowed"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)

	var body struct {
		Player   string `json:"player"`
		Username string `json:"username"`
		Value    int    `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid request body"))
		return
	}
	if body.Player == "" || body.Username == "" {
		writeJSON(w, http.StatusBadRequest, errorBody("player and username are required"))
		return
	}

	display, found := b.votes.FindPlayer(body.Player)
	if !found {
		writeJSON(w, http.StatusBadRequest, errorBody(fmt.Sprintf("player %q not found", body.Player)))
		return
	}

	prev, accepted := b.votes.CastVote(display, body.Username, body.Value)
	if !accepted {
		writeJSON(w, http.StatusBadRequest, errorBody(fmt.Sprintf("vote not accepted (voting closed or value out of range %d-%d)", b.cfg.MinVote, b.cfg.MaxVote)))
		return
	}

	b.logger.WithFields(logrus.Fields{
		"player":   display,
		"username": body.Username,
		"value":    body.Value,
	}).Info("Vote cast via HTTP bridge.")

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "player": display, "previous": prev, "value": body.Value})
}

// POST /api/votes/end
func (b *bridge) handleEnd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method not allowed"))
		return
	}
	tally := b.votes.CloseVoting()
	b.logger.WithField("results", tally.Results).Info("Voting closed via HTTP bridge.")
	writeJSON(w, http.StatusOK, tally)
}

// POST /api/votes/reset
func (b *bridge) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method not allowed"))
		return
	}
	b.votes.ResetVotes()
	b.logger.Info("Votes reset via HTTP bridge.")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// requireSecret wraps a handler to enforce the X-Bridge-Secret header.
// Only skipped when BRIDGE_AUTH_DISABLED=true, in which case start() is already
// bound to loopback so external callers can't reach here.
func (b *bridge) requireSecret(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if b.cfg.BridgeAuthDisabled {
			next(w, r)
			return
		}
		// Constant-time compare defends against timing side channels that could leak
		// the secret byte-by-byte. Length check first because ConstantTimeCompare only
		// runs in constant time when inputs are the same length.
		provided := r.Header.Get("X-Bridge-Secret")
		expected := b.cfg.BridgeSecret
		if len(provided) != len(expected) ||
			subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			b.logger.WithField("remote_addr", r.RemoteAddr).Warn("Bridge request rejected: invalid secret.")
			writeJSON(w, http.StatusForbidden, errorBody("forbidden"))
			return
		}
		next(w, r)
	}
}

// rateLimited returns a handler that rejects requests exceeding the global
// token-bucket rate. Applied to unauthenticated GET /api/votes so a single
// client can't hammer the endpoint.
func (b *bridge) rateLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !b.votesLimiter.Allow() {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, errorBody("rate limit exceeded"))
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errorBody(msg string) map[string]string {
	return map[string]string{"error": msg}
}
