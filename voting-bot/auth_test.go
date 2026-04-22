package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

// newTestTwitchClient builds a TwitchClient with quiet logging and overridable
// endpoint URLs — tests point helixAPI / tokenValidateURL / tokenRefreshURL at
// an httptest.Server.
func newTestTwitchClient(cfg *Config) *TwitchClient {
	if cfg.ClientID == "" {
		cfg.ClientID = "test-client-id"
	}
	if cfg.ClientSecret == "" {
		cfg.ClientSecret = "test-client-secret"
	}
	if cfg.BroadcasterUserID == "" {
		cfg.BroadcasterUserID = "1000"
	}
	if cfg.BotUserID == "" {
		cfg.BotUserID = "2000"
	}
	logger := logrus.New()
	logger.Out = io.Discard
	return &TwitchClient{
		cfg:              cfg,
		votes:            newVoteState(1, 5),
		logger:           logger,
		tokens:           newTokenStore("initial-access", "initial-refresh"),
		helixAPI:         defaultHelixAPI,
		tokenValidateURL: defaultTokenValidateURL,
		tokenRefreshURL:  defaultTokenRefreshURL,
		httpClient:       &http.Client{Timeout: 5 * time.Second},
		wsDialer:         &websocket.Dialer{HandshakeTimeout: 5 * time.Second},
	}
}

// TestDoAuth_HappyPath exercises the common case where the access token is
// valid on first try. No refresh should happen.
func TestDoAuth_HappyPath(t *testing.T) {
	var apiCalls, refreshCalls atomic.Int32

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer api.Close()

	refresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"access_token":"new","refresh_token":"new-refresh","expires_in":14400}`)
	}))
	defer refresh.Close()

	c := newTestTwitchClient(&Config{})
	c.helixAPI = api.URL
	c.tokenRefreshURL = refresh.URL

	resp, err := c.doAuth(context.Background(), http.MethodGet, c.helixAPI+"/users", nil)
	if err != nil {
		t.Fatalf("doAuth: unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
	if got := apiCalls.Load(); got != 1 {
		t.Errorf("api calls: got %d, want 1", got)
	}
	if got := refreshCalls.Load(); got != 0 {
		t.Errorf("refresh calls: got %d, want 0 (no refresh on happy path)", got)
	}
}

// TestDoAuth_401Retry confirms the retry-after-refresh behavior: first call
// returns 401, triggers refresh, retries with new token, second call succeeds.
func TestDoAuth_401Retry(t *testing.T) {
	var apiCalls, refreshCalls atomic.Int32
	var gotBearerTokens []string
	var bearerMu sync.Mutex

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		bearerMu.Lock()
		gotBearerTokens = append(gotBearerTokens, bearer)
		bearerMu.Unlock()

		n := apiCalls.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"unauthorized"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer api.Close()

	refresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":14400}`)
	}))
	defer refresh.Close()

	c := newTestTwitchClient(&Config{})
	c.helixAPI = api.URL
	c.tokenRefreshURL = refresh.URL

	resp, err := c.doAuth(context.Background(), http.MethodGet, c.helixAPI+"/users", nil)
	if err != nil {
		t.Fatalf("doAuth: unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("final status: got %d, want 200", resp.StatusCode)
	}
	if got := apiCalls.Load(); got != 2 {
		t.Errorf("api calls: got %d, want 2 (one 401 + one retry)", got)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Errorf("refresh calls: got %d, want 1", got)
	}
	bearerMu.Lock()
	defer bearerMu.Unlock()
	if len(gotBearerTokens) != 2 {
		t.Fatalf("bearer tokens captured: got %d, want 2", len(gotBearerTokens))
	}
	if gotBearerTokens[0] != "initial-access" {
		t.Errorf("first bearer: got %q, want initial-access", gotBearerTokens[0])
	}
	if gotBearerTokens[1] != "new-access" {
		t.Errorf("second bearer: got %q, want new-access (from refresh)", gotBearerTokens[1])
	}
}

// TestDoAuth_PersistentUnauthorized verifies that if the refreshed token is
// also rejected, doAuth returns the second 401 response verbatim without
// looping infinitely.
func TestDoAuth_PersistentUnauthorized(t *testing.T) {
	var apiCalls, refreshCalls atomic.Int32

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"unauthorized"}`)
	}))
	defer api.Close()

	refresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"access_token":"new","refresh_token":"new-refresh","expires_in":14400}`)
	}))
	defer refresh.Close()

	c := newTestTwitchClient(&Config{})
	c.helixAPI = api.URL
	c.tokenRefreshURL = refresh.URL

	resp, err := c.doAuth(context.Background(), http.MethodGet, c.helixAPI+"/users", nil)
	if err != nil {
		t.Fatalf("doAuth: unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401 (persistent auth failure)", resp.StatusCode)
	}
	if got := apiCalls.Load(); got != 2 {
		t.Errorf("api calls: got %d, want exactly 2 (no infinite retry)", got)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Errorf("refresh calls: got %d, want exactly 1", got)
	}
}

// TestRefreshToken_ConcurrentCallsDeduplicated verifies R2's fix: many parallel
// callers of refreshToken() must collapse onto a single upstream HTTP exchange,
// because Twitch rotates refresh tokens — the second request would POST an
// already-consumed refresh token and fail.
func TestRefreshToken_ConcurrentCallsDeduplicated(t *testing.T) {
	var refreshCalls atomic.Int32

	// The handler blocks briefly so concurrent callers have time to pile up on
	// singleflight. Without that delay the first caller might finish before the
	// second enters Do(), defeating the test.
	refresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"access_token":"new","refresh_token":"new-refresh","expires_in":14400}`)
	}))
	defer refresh.Close()

	c := newTestTwitchClient(&Config{})
	c.tokenRefreshURL = refresh.URL

	const goroutines = 10
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if err := c.refreshToken(context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("unexpected refresh error: %v", err)
	}

	if got := refreshCalls.Load(); got != 1 {
		t.Errorf("upstream refresh calls: got %d, want 1 (singleflight must dedupe)", got)
	}
	if tok := c.tokens.access(); tok != "new" {
		t.Errorf("access token after refresh: got %q, want 'new'", tok)
	}
}

// TestRefreshToken_ErrorPropagates confirms that a refresh failure surfaces to
// all concurrent callers (singleflight shares the error, not just the success).
func TestRefreshToken_ErrorPropagates(t *testing.T) {
	refresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
	}))
	defer refresh.Close()

	c := newTestTwitchClient(&Config{})
	c.tokenRefreshURL = refresh.URL

	err := c.refreshToken(context.Background())
	if err == nil {
		t.Fatal("refreshToken: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should mention status 400, got: %v", err)
	}
}

// TestValidateOnce_SetsExpiry confirms validate captures expires_in into the
// token store so refreshLoop's nextRefreshIn() works on the first iteration.
func TestValidateOnce_SetsExpiry(t *testing.T) {
	validate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"expires_in":14400}`)
	}))
	defer validate.Close()

	c := newTestTwitchClient(&Config{})
	c.tokenValidateURL = validate.URL

	if err := c.validateOnce(context.Background()); err != nil {
		t.Fatalf("validateOnce: %v", err)
	}

	// Not asserting the exact duration (time.Now advances mid-test) — just that
	// it's positive and in the right ballpark.
	until := c.tokens.timeUntilExpiry()
	if until <= 0 || until > 14400*time.Second {
		t.Errorf("timeUntilExpiry: got %v, want in (0, 14400s]", until)
	}
}

// TestValidate_401TriggersRefresh confirms a stale initial access token is
// self-healing: validate sees 401, refreshes, and re-validates.
func TestValidate_401TriggersRefresh(t *testing.T) {
	var validateCalls atomic.Int32

	validate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := validateCalls.Add(1)
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "OAuth ")
		if n == 1 {
			// First validate with the stale access token → 401
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Second validate with the refreshed access token → OK
		if bearer != "refreshed-access" {
			t.Errorf("second validate bearer: got %q, want refreshed-access", bearer)
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"expires_in":14400}`)
	}))
	defer validate.Close()

	refresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"access_token":"refreshed-access","refresh_token":"refreshed-refresh","expires_in":14400}`)
	}))
	defer refresh.Close()

	c := newTestTwitchClient(&Config{})
	c.tokenValidateURL = validate.URL
	c.tokenRefreshURL = refresh.URL

	if err := c.validate(context.Background()); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := validateCalls.Load(); got != 2 {
		t.Errorf("validate calls: got %d, want 2 (one 401 + one after refresh)", got)
	}
}

// TestNextRefreshIn confirms the 80%-of-lifetime math and the minimum-lead clamp.
func TestNextRefreshIn(t *testing.T) {
	tests := map[string]struct {
		expiresIn time.Duration
		wantMin   time.Duration
		wantMax   time.Duration
	}{
		"4 hour token": {
			// 80% of 4h = ~3h12m. Allow slack for the nanosecond the call takes.
			expiresIn: 4 * time.Hour,
			wantMin:   3*time.Hour + 10*time.Minute,
			wantMax:   3*time.Hour + 14*time.Minute,
		},
		"2 minute token": {
			// 80% of 2m = 1m36s, above refreshMinLead.
			expiresIn: 2 * time.Minute,
			wantMin:   1*time.Minute + 35*time.Second,
			wantMax:   1*time.Minute + 37*time.Second,
		},
		"30 second token clamps to minimum": {
			// 80% of 30s = 24s, below the 1-minute floor.
			expiresIn: 30 * time.Second,
			wantMin:   refreshMinLead,
			wantMax:   refreshMinLead,
		},
		"zero expiry clamps to minimum": {
			expiresIn: 0,
			wantMin:   refreshMinLead,
			wantMax:   refreshMinLead,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			c := newTestTwitchClient(&Config{})
			if test.expiresIn > 0 {
				c.tokens.setExpiry(test.expiresIn)
			}
			got := c.nextRefreshIn()
			if got < test.wantMin || got > test.wantMax {
				t.Errorf("nextRefreshIn: got %v, want in [%v, %v]", got, test.wantMin, test.wantMax)
			}
		})
	}
}
