package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	eventsubWSURL   = "wss://eventsub.wss.twitch.tv/ws"
	eventsubWSHost  = "eventsub.wss.twitch.tv"
	defaultHelixAPI = "https://api.twitch.tv/helix"

	reconnectDelay = 5 * time.Second

	httpClientTimeout   = 10 * time.Second
	wsHandshakeTimeout  = 10 * time.Second
	wsReadLimit         = 65536 // 64 KB... well above any EventSub message
	twitchMaxMessageLen = 500   // Twitch chat message character limit

	helixErrorBodyLimit = 2 * 1024
)

// errFatalHandling marks an error as unrecoverable from run()'s perspective —
// e.g. EventSub subscription registration failed for a non-auth reason. Wrapped
// with %w so errors.Is lets run() distinguish "should reconnect" from "should exit".
var errFatalHandling = errors.New("fatal EventSub handling")

// Twitch EventSub message types

type wsMessage struct {
	Metadata struct {
		MessageType      string `json:"message_type"`
		SubscriptionType string `json:"subscription_type"`
	} `json:"metadata"`
	Payload struct {
		Session struct {
			ID           string `json:"id"`
			ReconnectURL string `json:"reconnect_url"`
		} `json:"session"`
		Event chatMessageEvent `json:"event"`
	} `json:"payload"`
}

type chatMessageEvent struct {
	BroadcasterUserLogin string `json:"broadcaster_user_login"`
	BroadcasterUserID    string `json:"broadcaster_user_id"`
	ChatterUserLogin     string `json:"chatter_user_login"`
	ChatterUserID        string `json:"chatter_user_id"`
	Message              struct {
		Text string `json:"text"`
	} `json:"message"`
	Badges []struct {
		SetID string `json:"set_id"`
	} `json:"badges"`
}

func (ev *chatMessageEvent) isModerator() bool {
	for _, b := range ev.Badges {
		if b.SetID == "moderator" {
			return true
		}
	}
	return false
}

// Client

type TwitchClient struct {
	cfg        *Config
	votes      *VoteState
	logger     *logrus.Logger
	httpClient *http.Client
	wsDialer   *websocket.Dialer
	tokens     *tokenStore

	// Twitch endpoint URLs are fields rather than consts so tests can point the
	// client at an httptest.Server.
	helixAPI         string
	tokenValidateURL string
	tokenRefreshURL  string

	// Dedupes concurrent refreshToken() callers (proactive refreshLoop vs a
	// reactive 401 retry inside doAuth). See auth.go:refreshToken for detail.
	refreshGroup singleflight.Group

	reconnectURL string // set on session_reconnect
}

func newTwitchClient(cfg *Config, votes *VoteState, logger *logrus.Logger) *TwitchClient {
	return &TwitchClient{
		cfg:              cfg,
		votes:            votes,
		logger:           logger,
		tokens:           newTokenStore(cfg.OAuthToken, cfg.RefreshToken),
		helixAPI:         defaultHelixAPI,
		tokenValidateURL: defaultTokenValidateURL,
		tokenRefreshURL:  defaultTokenRefreshURL,
		httpClient: &http.Client{
			Timeout: httpClientTimeout,
		},
		wsDialer: &websocket.Dialer{
			HandshakeTimeout: wsHandshakeTimeout,
		},
	}
}

// run connects to EventSub and reconnects indefinitely on transient failure.
// Exits cleanly on ctx cancellation. Returns a non-nil error only for fatal
// conditions (e.g. non-401 subscription registration failure), which main
// surfaces before shutting down.
func (c *TwitchClient) run(ctx context.Context) error {
	wsURL := eventsubWSURL
	for {
		if ctx.Err() != nil {
			return nil
		}

		err := c.connect(ctx, wsURL)

		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errFatalHandling) {
			return err
		}

		if c.reconnectURL != "" {
			c.logger.WithField("url", c.reconnectURL).Info("Reconnecting to new EventSub URL.")
			wsURL = c.reconnectURL
			c.reconnectURL = ""
			continue
		}

		if err != nil {
			c.logger.WithError(err).Warnf("EventSub connection lost, retrying in %s.", reconnectDelay)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(reconnectDelay):
		}
		wsURL = eventsubWSURL
	}
}

// connect opens one WebSocket session. Returns when the connection closes, a
// session_reconnect is received (c.reconnectURL is set), ctx is cancelled, or
// a fatal error bubbles up from message handling.
func (c *TwitchClient) connect(ctx context.Context, wsURL string) error {
	conn, _, err := c.wsDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("failed to dial EventSub WebSocket: %w", err)
	}
	defer conn.Close()

	// Cap inbound message size. Twitch EventSub messages are small. this should
	// prevent a misbehaving or spoofed server from pushing arbitrarily large payloads.
	conn.SetReadLimit(wsReadLimit)

	// Close the conn when ctx is cancelled so the blocked ReadMessage returns.
	// The done channel prevents this helper goroutine from outliving connect().
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	c.logger.WithField("url", wsURL).Info("Connected to EventSub WebSocket.")

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("failed to read from EventSub WebSocket: %w", err)
		}

		var msg wsMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			c.logger.WithError(err).Warn("Received malformed EventSub message, skipping.")
			continue
		}

		reconnect, err := c.handleMessage(ctx, &msg)
		if err != nil {
			return err
		}
		if reconnect {
			return nil // caller will use c.reconnectURL
		}
	}
}

// handleMessage processes one EventSub message. Returns (reconnect, err). A
// non-nil err wrapping errFatalHandling signals run() to exit.
func (c *TwitchClient) handleMessage(ctx context.Context, msg *wsMessage) (bool, error) {
	switch msg.Metadata.MessageType {
	case "session_welcome":
		sessionID := msg.Payload.Session.ID
		c.logger.WithField("session_id", sessionID).Info("EventSub session established.")
		// We can't operate without an EventSub subscription — chat messages won't
		// arrive. So any non-auth failure here is terminal; main will log and exit.
		if err := c.registerEventSubListeners(ctx, sessionID); err != nil {
			return false, fmt.Errorf("%w: register EventSub listeners: %w", errFatalHandling, err)
		}

	case "session_keepalive":
		// no-op

	case "notification":
		if msg.Metadata.SubscriptionType == "channel.chat.message" {
			c.handleChatMessage(ctx, &msg.Payload.Event)
		}

	case "session_reconnect":
		raw := msg.Payload.Session.ReconnectURL
		if !isValidReconnectURL(raw) {
			// Reconnect URLs must come from Twitch's own EventSub host. Anything
			// else could be an attempt to redirect the bot to an attacker-controlled server.
			c.logger.WithField("url", raw).Warn("Ignoring session_reconnect with unrecognised URL, will reconnect to default.")
			return false, nil
		}
		c.reconnectURL = raw
		return true, nil
	}

	return false, nil
}

// isValidReconnectURL ensures a Twitch-issued reconnect URL stays on the expected host.
func isValidReconnectURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Scheme == "wss" && u.Host == eventsubWSHost
}

// Chat command handling

const (
	maxPlayers    = 13
	maxPlayerName = 25
)

var (
	voteRe       = regexp.MustCompile(`(?i)^!vote\s+(\S+)\s+(\d+)$`)
	playerNameRe = regexp.MustCompile(`^[\w]+$`)
)

// validatePlayerNames checks for duplicates, length, and allowed characters.
func validatePlayerNames(players []string) error {
	seen := make(map[string]bool, len(players))
	for _, p := range players {
		if len(p) > maxPlayerName {
			return fmt.Errorf("player name %q is too long (max %d chars)", p, maxPlayerName)
		}
		if !playerNameRe.MatchString(p) {
			return fmt.Errorf("player name %q contains invalid characters", p)
		}
		key := strings.ToLower(p)
		if seen[key] {
			return fmt.Errorf("duplicate player name: %s", p)
		}
		seen[key] = true
	}
	return nil
}

func (c *TwitchClient) handleChatMessage(ctx context.Context, ev *chatMessageEvent) {
	text := strings.TrimSpace(ev.Message.Text)
	username := ev.ChatterUserLogin
	isBroadcaster := ev.ChatterUserID == ev.BroadcasterUserID
	isPrivileged := isBroadcaster || ev.isModerator()

	c.logger.WithFields(logrus.Fields{
		"channel": ev.BroadcasterUserLogin,
		"user":    username,
		"message": text,
	}).Debug("Chat message received.")

	// Broadcaster and moderator commands
	if isPrivileged {
		if strings.HasPrefix(text, "!startvote") {
			players := strings.Fields(strings.TrimPrefix(text, "!startvote"))
			if len(players) == 0 {
				c.sendChatMessage(ctx, "Usage: !startvote player1 player2 ...")
				return
			}
			if len(players) > maxPlayers {
				c.sendChatMessage(ctx, fmt.Sprintf("Too many players (max %d).", maxPlayers))
				return
			}
			if err := validatePlayerNames(players); err != nil {
				c.sendChatMessage(ctx, err.Error())
				return
			}
			c.votes.OpenVoting(players)
			c.logger.WithFields(logrus.Fields{
				"players": players,
				"range":   fmt.Sprintf("%d-%d", c.cfg.MinVote, c.cfg.MaxVote),
			}).Info("Voting opened.")

			c.sendChatMessage(ctx, fmt.Sprintf(
				"Voting is now OPEN! Players: %s. Type !vote <player> <%d-%d>",
				strings.Join(players, ", "), c.cfg.MinVote, c.cfg.MaxVote,
			))
			return
		}

		if text == "!endvote" {
			if !c.votes.IsOpen() && !c.votes.HasVotes() {
				c.sendChatMessage(ctx, "No vote is currently open.")
				return
			}
			tally := c.votes.CloseVoting()
			c.logger.WithField("results", tally.Results).Info("Voting closed.")
			c.sendChatMessage(ctx, "Voting closed! "+strings.Join(formatResultLines(tally, c.cfg.MaxVote), " | "))
			return
		}

		if text == "!resetvote" {
			c.votes.ResetVotes()
			c.logger.Info("Votes reset.")
			c.sendChatMessage(ctx, "Votes have been reset.")
			return
		}
	}

	// Anyone: show current tally
	if text == "!votes" || text == "!votecount" {
		if !c.votes.IsOpen() && !c.votes.HasVotes() {
			c.sendChatMessage(ctx, "No vote is currently open.")
			return
		}
		tally := c.votes.GetTally()
		status := "OPEN"
		if !tally.Open {
			status = "CLOSED"
		}
		lines := make([]string, 0, len(tally.Results))
		for _, r := range tally.Results {
			avg := "n/a"
			if r.Average != nil {
				avg = fmt.Sprintf("%.1f", *r.Average)
			}
			lines = append(lines, fmt.Sprintf("%s: %s/%d (%d votes)", r.Player, avg, c.cfg.MaxVote, r.Total))
		}
		c.sendChatMessage(ctx, fmt.Sprintf("[%s] %s", status, strings.Join(lines, " | ")))
		return
	}

	// Vote command
	if c.votes.IsOpen() {
		if m := voteRe.FindStringSubmatch(text); m != nil {
			playerInput := m[1]
			val, _ := strconv.Atoi(m[2])

			display, found := c.votes.FindPlayer(playerInput)
			if !found {
				playerList := strings.Join(c.votes.GetTally().Players, ", ")
				c.sendChatMessage(ctx, fmt.Sprintf(
					"@%s Player %q not found. Players: %s",
					username, playerInput, playerList,
				))
				return
			}

			prev, accepted := c.votes.CastVote(display, username, val)
			if !accepted {
				c.sendChatMessage(ctx, fmt.Sprintf(
					"@%s Vote must be between %d and %d.",
					username, c.cfg.MinVote, c.cfg.MaxVote,
				))
				return
			}
			if prev != 0 {
				c.logger.WithFields(logrus.Fields{
					"user":   username,
					"player": display,
					"from":   prev,
					"to":     val,
				}).Info("Vote changed.")
			} else {
				c.logger.WithFields(logrus.Fields{
					"user":   username,
					"player": display,
					"vote":   val,
				}).Info("Vote cast.")
			}
		}
	}
}

// Twitch API helpers

// doAuth sends a Helix request with the current bearer token. On 401 it
// refreshes the token and retries once. Non-401 responses (including non-2xx
// like 4xx payload errors) are returned verbatim so the caller can inspect
// status and body.
func (c *TwitchClient) doAuth(ctx context.Context, method, endpoint string, body []byte) (*http.Response, error) {
	resp, err := c.sendHelix(ctx, method, endpoint, body)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}

	// 401: the access token probably expired between the refresh loop's
	// wakeups (Twitch revoked it, clock skew, race). Drain the body so the
	// http.Client can reuse the connection, refresh, retry once.
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	c.logger.Warn("Helix API returned 401, attempting token refresh.")
	if rerr := c.refreshToken(ctx); rerr != nil {
		return nil, fmt.Errorf("refresh after 401 failed: %w", rerr)
	}
	return c.sendHelix(ctx, method, endpoint, body)
}

// sendHelix is the single-shot authenticated send; doAuth layers retry on top.
// Split out so the retry path is a straight-line call pair instead of a loop
// with an unreachable fallthrough.
func (c *TwitchClient) sendHelix(ctx context.Context, method, endpoint string, body []byte) (*http.Response, error) {
	var reqBody io.Reader
	if len(body) > 0 {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to build Helix request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.tokens.access())
	req.Header.Set("Client-Id", c.cfg.ClientID)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Helix request failed: %w", err)
	}
	return resp, nil
}

func (c *TwitchClient) sendChatMessage(ctx context.Context, message string) {
	message = truncateMessage(message)

	body, err := json.Marshal(map[string]string{
		"broadcaster_id": c.cfg.BroadcasterUserID,
		"sender_id":      c.cfg.BotUserID,
		"message":        message,
	})
	if err != nil {
		c.logger.WithError(err).Error("Failed to marshal chat message body.")
		return
	}

	resp, err := c.doAuth(ctx, http.MethodPost, c.helixAPI+"/chat/messages", body)
	if err != nil {
		c.logger.WithError(err).Error("Failed to send chat message.")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, helixErrorBodyLimit))
		c.logger.WithFields(logrus.Fields{
			"status":   resp.StatusCode,
			"response": string(errBody),
		}).Error("Chat message rejected by Twitch API.")
		return
	}

	c.logger.WithField("message", message).Debug("Chat message sent.")
}

func (c *TwitchClient) registerEventSubListeners(ctx context.Context, sessionID string) error {
	body, err := json.Marshal(map[string]any{
		"type":    "channel.chat.message",
		"version": "1",
		"condition": map[string]string{
			"broadcaster_user_id": c.cfg.BroadcasterUserID,
			"user_id":             c.cfg.BotUserID,
		},
		"transport": map[string]string{
			"method":     "websocket",
			"session_id": sessionID,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to marshal EventSub subscription body: %w", err)
	}

	resp, err := c.doAuth(ctx, http.MethodPost, c.helixAPI+"/eventsub/subscriptions", body)
	if err != nil {
		return fmt.Errorf("failed to register EventSub subscription: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, helixErrorBodyLimit))
		return fmt.Errorf("failed to register EventSub subscription, status %d: %s", resp.StatusCode, errBody)
	}

	var result struct {
		Data []struct{ ID string } `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		c.logger.WithError(err).Warn("Failed to decode EventSub subscription response, continuing.")
		return nil
	}
	if len(result.Data) > 0 {
		c.logger.WithField("subscription_id", result.Data[0].ID).Info("Subscribed to channel chat messages.")
	}
	return nil
}

func formatResultLines(tally Tally, maxVote int) []string {
	lines := make([]string, 0, len(tally.Results))
	for i, r := range tally.Results {
		avg := "n/a"
		if r.Average != nil {
			avg = fmt.Sprintf("%.1f", *r.Average)
		}
		var line string
		if r.Average != nil {
			switch i {
			case 0:
				line = fmt.Sprintf("👑 %s ate and left no crumbs %s/%d (%d votes)", r.Player, avg, maxVote, r.Total)
			case 1:
				line = fmt.Sprintf("💅 %s served... but not quite enough %s/%d (%d votes)", r.Player, avg, maxVote, r.Total)
			case 2:
				line = fmt.Sprintf("👀 %s had potential %s/%d (%d votes)", r.Player, avg, maxVote, r.Total)
			default:
				line = fmt.Sprintf("%s: %s/%d (%d votes)", r.Player, avg, maxVote, r.Total)
			}
		} else {
			line = fmt.Sprintf("%s: n/a/%d (0 votes)", r.Player, maxVote)
		}
		lines = append(lines, line)
	}
	return lines
}

// truncateMessage ensures a message fits within Twitch's 500 character limit.
func truncateMessage(msg string) string {
	if utf8.RuneCountInString(msg) <= twitchMaxMessageLen {
		return msg
	}
	runes := []rune(msg)
	return string(runes[:twitchMaxMessageLen-1]) + "…"
}
