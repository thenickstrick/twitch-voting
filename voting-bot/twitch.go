package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

const (
	eventsubWSURL  = "wss://eventsub.wss.twitch.tv/ws"
	eventsubWSHost = "eventsub.wss.twitch.tv"
	twitchAPI      = "https://api.twitch.tv/helix"

	reconnectDelay = 5 * time.Second

	httpClientTimeout   = 10 * time.Second
	wsHandshakeTimeout  = 10 * time.Second
	wsReadLimit         = 65536 // 64 KB... well above any EventSub message
	twitchMaxMessageLen = 500   // Twitch chat message character limit
)

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
	cfg          *Config
	votes        *VoteState
	logger       *logrus.Logger
	httpClient   *http.Client
	wsDialer     *websocket.Dialer
	reconnectURL string // set on session_reconnect
}

func newTwitchClient(cfg *Config, votes *VoteState, logger *logrus.Logger) *TwitchClient {
	return &TwitchClient{
		cfg:    cfg,
		votes:  votes,
		logger: logger,
		httpClient: &http.Client{
			Timeout: httpClientTimeout,
		},
		wsDialer: &websocket.Dialer{
			HandshakeTimeout: wsHandshakeTimeout,
		},
	}
}

// validateAuth calls the Twitch token validation endpoint.
func (c *TwitchClient) validateAuth() error {
	req, _ := http.NewRequest(http.MethodGet, "https://id.twitch.tv/oauth2/validate", nil)
	req.Header.Set("Authorization", "OAuth "+c.cfg.OAuthToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to validate auth token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return fmt.Errorf("failed to validate auth token, status %d: %v", resp.StatusCode, body)
	}

	c.logger.Info("Twitch auth token validated.")
	return nil
}

// run connects to EventSub and reconnects indefinitely on failure.
func (c *TwitchClient) run() {
	url := eventsubWSURL
	for {
		err := c.connect(url)
		if c.reconnectURL != "" {
			c.logger.WithField("url", c.reconnectURL).Info("Reconnecting to new EventSub URL.")
			url = c.reconnectURL
			c.reconnectURL = ""
		} else {
			if err != nil {
				c.logger.WithError(err).Warnf("EventSub connection lost, retrying in %s.", reconnectDelay)
			}
			time.Sleep(reconnectDelay)
			url = eventsubWSURL
		}
	}
}

// connect opens one WebSocket session. Returns when the connection closes or
// a session_reconnect is received (in which case c.reconnectURL is set).
func (c *TwitchClient) connect(wsURL string) error {
	conn, _, err := c.wsDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("failed to dial EventSub WebSocket: %w", err)
	}
	defer conn.Close()

	// Cap inbound message size. Twitch EventSub messages are small. this should
	// prevent a misbehaving or spoofed server from pushing arbitrarily large payloads.
	conn.SetReadLimit(wsReadLimit)

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

		if done := c.handleMessage(&msg); done {
			return nil // caller will use c.reconnectURL
		}
	}
}

// handleMessage processes one EventSub message.
// Returns true when the caller should stop reading and reconnect.
func (c *TwitchClient) handleMessage(msg *wsMessage) bool {
	switch msg.Metadata.MessageType {
	case "session_welcome":
		sessionID := msg.Payload.Session.ID
		c.logger.WithField("session_id", sessionID).Info("EventSub session established.")
		if err := c.registerEventSubListeners(sessionID); err != nil {
			c.logger.WithError(err).Fatal("Failed to register EventSub listeners for dti-chatbot.")
		}

	case "session_keepalive":
		// no-op

	case "notification":
		if msg.Metadata.SubscriptionType == "channel.chat.message" {
			c.handleChatMessage(&msg.Payload.Event)
		}

	case "session_reconnect":
		raw := msg.Payload.Session.ReconnectURL
		if !isValidReconnectURL(raw) {
			// Reconnect URLs must come from Twitch's own EventSub host. Anything
			// else could be an attempt to redirect the bot to an attacker-controlled server.
			c.logger.WithField("url", raw).Warn("Ignoring session_reconnect with unrecognised URL, will reconnect to default.")
			return false
		}
		c.reconnectURL = raw
		return true
	}

	return false
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
	voteRe      = regexp.MustCompile(`(?i)^!vote\s+(\S+)\s+(\d+)$`)
	playerNameRe = regexp.MustCompile(`^[\w]+$`)
)

// validatePlayerNames checks for duplicates, length, and allowed characters.
func validatePlayerNames(players []string) error {
	seen := make(map[string]bool, len(players))
	for _, p := range players {
		if len(p) > maxPlayerName {
			return fmt.Errorf("Player name %q is too long (max %d chars).", p, maxPlayerName)
		}
		if !playerNameRe.MatchString(p) {
			return fmt.Errorf("Player name %q contains invalid characters.", p)
		}
		key := strings.ToLower(p)
		if seen[key] {
			return fmt.Errorf("Duplicate player name: %s", p)
		}
		seen[key] = true
	}
	return nil
}

func (c *TwitchClient) handleChatMessage(ev *chatMessageEvent) {
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
				c.sendChatMessage("Usage: !startvote player1 player2 ...")
				return
			}
			if len(players) > maxPlayers {
				c.sendChatMessage(fmt.Sprintf("Too many players (max %d).", maxPlayers))
				return
			}
			if err := validatePlayerNames(players); err != nil {
				c.sendChatMessage(err.Error())
				return
			}
			c.votes.OpenVoting(players)
			c.logger.WithFields(logrus.Fields{
				"players": players,
				"range":   fmt.Sprintf("%d-%d", c.cfg.MinVote, c.cfg.MaxVote),
			}).Info("Voting opened.")

			c.sendChatMessage(fmt.Sprintf(
				"Voting is now OPEN! Players: %s. Type !vote <player> <%d-%d>",
				strings.Join(players, ", "), c.cfg.MinVote, c.cfg.MaxVote,
			))
			return
		}

		if text == "!endvote" {
			if !c.votes.IsOpen() && !c.votes.HasVotes() {
				c.sendChatMessage("No vote is currently open.")
				return
			}
			tally := c.votes.CloseVoting()
			c.logger.WithField("results", tally.Results).Info("Voting closed.")
			c.sendChatMessage("Voting closed! " + strings.Join(formatResultLines(tally, c.cfg.MaxVote), " | "))
			return
		}

		if text == "!resetvote" {
			c.votes.ResetVotes()
			c.logger.Info("Votes reset.")
			c.sendChatMessage("Votes have been reset.")
			return
		}
	}

	// Anyone: show current tally
	if text == "!votes" || text == "!votecount" {
		if !c.votes.IsOpen() && !c.votes.HasVotes() {
			c.sendChatMessage("No vote is currently open.")
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
		c.sendChatMessage(fmt.Sprintf("[%s] %s", status, strings.Join(lines, " | ")))
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
				c.sendChatMessage(fmt.Sprintf(
					"@%s Player %q not found. Players: %s",
					username, playerInput, playerList,
				))
				return
			}

			prev, accepted := c.votes.CastVote(display, username, val)
			if !accepted {
				c.sendChatMessage(fmt.Sprintf(
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

func (c *TwitchClient) sendChatMessage(message string) {
	message = truncateMessage(message)

	body, _ := json.Marshal(map[string]string{
		"broadcaster_id": c.cfg.BroadcasterUserID,
		"sender_id":      c.cfg.BotUserID,
		"message":        message,
	})

	req, _ := http.NewRequest(http.MethodPost, twitchAPI+"/chat/messages", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.cfg.OAuthToken)
	req.Header.Set("Client-Id", c.cfg.ClientID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.WithError(err).Error("Failed to send chat message.")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		c.logger.WithFields(logrus.Fields{
			"status":   resp.StatusCode,
			"response": errBody,
		}).Error("Chat message rejected by Twitch API.")
		return
	}

	c.logger.WithField("message", message).Debug("Chat message sent.")
}

func (c *TwitchClient) registerEventSubListeners(sessionID string) error {
	body, _ := json.Marshal(map[string]any{
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

	req, _ := http.NewRequest(http.MethodPost, twitchAPI+"/eventsub/subscriptions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.cfg.OAuthToken)
	req.Header.Set("Client-Id", c.cfg.ClientID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to register EventSub subscription: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		var errBody map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		return fmt.Errorf("failed to register EventSub subscription, status %d: %v", resp.StatusCode, errBody)
	}

	var result struct {
		Data []struct{ ID string } `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&result)
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
