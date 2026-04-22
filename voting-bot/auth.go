package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	defaultTokenValidateURL = "https://id.twitch.tv/oauth2/validate"
	defaultTokenRefreshURL  = "https://id.twitch.tv/oauth2/token"

	// Refresh proactively once 80% of the access-token lifetime has passed; this
	// leaves a 20% buffer for transient failures without burning through
	// refreshes. Minimum lead prevents thrashing if a token arrives near expiry.
	refreshLeadFraction = 0.8
	refreshMinLead      = 1 * time.Minute
	refreshRetryBackoff = 30 * time.Second
	// Cap for exponential backoff on repeated refresh failures. A permanently
	// revoked refresh token would otherwise spam the logs every 30s forever.
	refreshMaxBackoff = 5 * time.Minute

	// Cap for error-body reads so a misbehaving upstream can't make us log
	// megabytes of noise into journald.
	authErrorBodyLimit = 2 * 1024

	// singleflight key for the refresh path. All concurrent refresh callers
	// collapse onto one in-flight HTTP request regardless of call site.
	refreshSingleflightKey = "refresh"
)

var errTokenUnauthorized = errors.New("access token unauthorized")

// tokenStore holds the live Twitch access token and its expiry, plus the refresh
// token used to mint new ones. All methods are safe for concurrent use.
type tokenStore struct {
	mu           sync.RWMutex
	accessToken  string
	refreshToken string
	expiresAt    time.Time
}

func newTokenStore(access, refresh string) *tokenStore {
	return &tokenStore{
		accessToken:  access,
		refreshToken: refresh,
	}
}

func (ts *tokenStore) access() string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.accessToken
}

func (ts *tokenStore) refresh() string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.refreshToken
}

func (ts *tokenStore) timeUntilExpiry() time.Duration {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	if ts.expiresAt.IsZero() {
		return 0
	}
	return time.Until(ts.expiresAt)
}

// setExpiry updates the access-token expiry without rotating the token itself.
// Used after /oauth2/validate reports the current token's remaining lifetime.
func (ts *tokenStore) setExpiry(expiresIn time.Duration) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.expiresAt = time.Now().Add(expiresIn)
}

// setRefreshed replaces the access token (and refresh token if rotated) and
// resets the expiry. Called after a successful refresh.
func (ts *tokenStore) setRefreshed(access, refresh string, expiresIn time.Duration) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if access != "" {
		ts.accessToken = access
	}
	if refresh != "" {
		ts.refreshToken = refresh
	}
	ts.expiresAt = time.Now().Add(expiresIn)
}

type validateResponse struct {
	ExpiresIn int `json:"expires_in"`
}

// validate confirms the current access token is live and captures its remaining
// lifetime. On 401 it refreshes once and retries — the initial token from .env
// may already be stale if the process is restarted after ~4h of downtime.
func (c *TwitchClient) validate(ctx context.Context) error {
	err := c.validateOnce(ctx)
	if err == nil {
		return nil
	}
	if !errors.Is(err, errTokenUnauthorized) {
		return err
	}

	c.logger.Warn("Initial OAuth access token rejected, attempting refresh.")
	if rerr := c.refreshToken(ctx); rerr != nil {
		return fmt.Errorf("refresh after initial 401 failed: %w", rerr)
	}
	return c.validateOnce(ctx)
}

func (c *TwitchClient) validateOnce(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.tokenValidateURL, nil)
	if err != nil {
		return fmt.Errorf("failed to build auth validate request: %w", err)
	}
	req.Header.Set("Authorization", "OAuth "+c.tokens.access())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to validate auth token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return errTokenUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, authErrorBodyLimit))
		return fmt.Errorf("failed to validate auth token, status %d: %s", resp.StatusCode, body)
	}

	var res validateResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return fmt.Errorf("failed to decode validate response: %w", err)
	}

	c.tokens.setExpiry(time.Duration(res.ExpiresIn) * time.Second)
	c.logger.WithField("expires_in_sec", res.ExpiresIn).Info("Twitch auth token validated.")
	return nil
}

type refreshResponse struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	ExpiresIn    int      `json:"expires_in"`
	Scope        []string `json:"scope"`
	TokenType    string   `json:"token_type"`
}

// refreshToken is the serialized, deduplicated entry point for refreshing the
// access token. refreshLoop and doAuth's 401 retry both call it; singleflight
// ensures only one HTTP exchange reaches Twitch even under concurrent callers.
// Without this, two parallel refreshes would race: Twitch rotates the refresh
// token on each use, so the second request would POST a now-invalid token and
// fail with invalid_grant — masking the real state of the store.
func (c *TwitchClient) refreshToken(ctx context.Context) error {
	_, err, _ := c.refreshGroup.Do(refreshSingleflightKey, func() (any, error) {
		return nil, c.doRefresh(ctx)
	})
	return err
}

func (c *TwitchClient) doRefresh(ctx context.Context) error {
	current := c.tokens.refresh()

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", current)
	form.Set("client_id", c.cfg.ClientID)
	form.Set("client_secret", c.cfg.ClientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenRefreshURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("failed to build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to refresh auth token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, authErrorBodyLimit))
		return fmt.Errorf("refresh request failed, status %d: %s", resp.StatusCode, body)
	}

	var refreshed refreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&refreshed); err != nil {
		return fmt.Errorf("failed to decode refresh response: %w", err)
	}

	c.tokens.setRefreshed(refreshed.AccessToken, refreshed.RefreshToken,
		time.Duration(refreshed.ExpiresIn)*time.Second)

	c.logger.WithFields(logrus.Fields{
		"expires_in_sec": refreshed.ExpiresIn,
	}).Info("OAuth access token refreshed.")

	// Twitch rotates the refresh token under some grant flows. When that happens
	// the old REFRESH_TOKEN in .env will fail on the next process start — warn
	// loudly so an operator can copy the new value.
	if refreshed.RefreshToken != "" && refreshed.RefreshToken != current {
		c.logger.Warn("Twitch rotated the refresh token; persist the new REFRESH_TOKEN before the next restart.")
	}
	return nil
}

// refreshLoop proactively refreshes the access token ahead of expiry.
//
// Ordering invariant: main must call validate() synchronously before spawning
// this goroutine. validate() is what populates tokenStore.expiresAt with the
// actual remaining lifetime; without it, nextRefreshIn() sees expiresAt=0 and
// clamps to refreshMinLead, so the loop would tick every 1 minute forever.
//
// On refresh failure, backoff doubles up to refreshMaxBackoff so a permanently
// revoked refresh token doesn't flood the logs; it resets after the next
// successful refresh.
func (c *TwitchClient) refreshLoop(ctx context.Context) {
	backoff := refreshRetryBackoff
	for {
		wait := c.nextRefreshIn()
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		if err := c.refreshToken(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			c.logger.WithError(err).
				WithField("retry_in_sec", int(backoff.Seconds())).
				Error("Failed to refresh OAuth access token.")
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > refreshMaxBackoff {
				backoff = refreshMaxBackoff
			}
			continue
		}
		backoff = refreshRetryBackoff
	}
}

func (c *TwitchClient) nextRefreshIn() time.Duration {
	until := c.tokens.timeUntilExpiry()
	lead := time.Duration(float64(until) * refreshLeadFraction)
	if lead < refreshMinLead {
		lead = refreshMinLead
	}
	return lead
}
