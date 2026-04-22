package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	serverReadHeaderTimeout = 2 * time.Second
	serverReadTimeout       = 5 * time.Second
	serverWriteTimeout      = 10 * time.Second
	serverIdleTimeout       = 60 * time.Second
	maxRequestBodyBytes     = 1024
)

type bridge struct {
	cfg    *Config
	votes  *VoteState
	logger *logrus.Logger
}

func newBridge(cfg *Config, votes *VoteState, logger *logrus.Logger) *bridge {
	return &bridge{cfg: cfg, votes: votes, logger: logger}
}

func (b *bridge) start() {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/votes", b.handleVotes)
	mux.HandleFunc("/api/votes/detail", b.requireSecret(b.handleDetail))
	mux.HandleFunc("/api/votes/start", b.requireSecret(b.handleStart))
	mux.HandleFunc("/api/votes/cast", b.requireSecret(b.handleCast))
	mux.HandleFunc("/api/votes/end", b.requireSecret(b.handleEnd))
	mux.HandleFunc("/api/votes/reset", b.requireSecret(b.handleReset))

	addr := ":" + b.cfg.BridgePort
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		ReadTimeout:       serverReadTimeout,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
	}

	b.logger.WithField("addr", addr).Info("HTTP bridge listening.")

	if err := server.ListenAndServe(); err != nil {
		b.logger.WithError(err).Fatal("HTTP bridge server stopped unexpectedly.")
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
// If BRIDGE_SECRET is empty the check is skipped (dev mode).
func (b *bridge) requireSecret(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if b.cfg.BridgeSecret != "" && r.Header.Get("X-Bridge-Secret") != b.cfg.BridgeSecret {
			b.logger.WithField("remote_addr", r.RemoteAddr).Warn("Bridge request rejected: invalid secret.")
			writeJSON(w, http.StatusForbidden, errorBody("forbidden"))
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
