package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

func newTestBridge(cfg *Config) *bridge {
	if cfg.MinVote == 0 && cfg.MaxVote == 0 {
		cfg.MinVote, cfg.MaxVote = 1, 5
	}
	logger := logrus.New()
	logger.Out = io.Discard // keep test output clean
	return newBridge(cfg, newVoteState(cfg.MinVote, cfg.MaxVote), logger)
}

func TestRequireSecret(t *testing.T) {
	tests := map[string]struct {
		bridgeSecret       string
		bridgeAuthDisabled bool
		header             string
		sendHeader         bool
		wantStatus         int
	}{
		"valid secret": {
			bridgeSecret: "s3cret", header: "s3cret", sendHeader: true,
			wantStatus: http.StatusOK,
		},
		"missing header": {
			bridgeSecret: "s3cret", sendHeader: false,
			wantStatus: http.StatusForbidden,
		},
		"wrong secret same length": {
			bridgeSecret: "s3cret", header: "notit!", sendHeader: true,
			wantStatus: http.StatusForbidden,
		},
		"wrong secret shorter": {
			bridgeSecret: "s3cret", header: "s", sendHeader: true,
			wantStatus: http.StatusForbidden,
		},
		"wrong secret longer": {
			bridgeSecret: "s3cret", header: "s3cretXXXX", sendHeader: true,
			wantStatus: http.StatusForbidden,
		},
		"auth disabled bypasses check": {
			bridgeAuthDisabled: true, sendHeader: false,
			wantStatus: http.StatusOK,
		},
		"auth disabled with stale secret": {
			bridgeSecret: "s3cret", bridgeAuthDisabled: true, sendHeader: false,
			wantStatus: http.StatusOK,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			b := newTestBridge(&Config{
				BridgeSecret:       test.bridgeSecret,
				BridgeAuthDisabled: test.bridgeAuthDisabled,
			})
			handler := b.requireSecret(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if test.sendHeader {
				req.Header.Set("X-Bridge-Secret", test.header)
			}
			rec := httptest.NewRecorder()
			handler(rec, req)

			if rec.Code != test.wantStatus {
				t.Errorf("status: got %d, want %d", rec.Code, test.wantStatus)
			}
		})
	}
}

func TestHandleStart(t *testing.T) {
	tests := map[string]struct {
		method     string
		body       string
		wantStatus int
		wantOpen   bool // expect voting open after the call
	}{
		"valid single player": {
			method: http.MethodPost, body: `{"players":["Alice"]}`,
			wantStatus: http.StatusOK, wantOpen: true,
		},
		"valid multiple players": {
			method: http.MethodPost, body: `{"players":["Alice","Bob","Charlie"]}`,
			wantStatus: http.StatusOK, wantOpen: true,
		},
		"wrong method": {
			method: http.MethodGet, body: "",
			wantStatus: http.StatusMethodNotAllowed,
		},
		"invalid JSON": {
			method: http.MethodPost, body: `not json`,
			wantStatus: http.StatusBadRequest,
		},
		"empty players array": {
			method: http.MethodPost, body: `{"players":[]}`,
			wantStatus: http.StatusBadRequest,
		},
		"missing players field": {
			method: http.MethodPost, body: `{}`,
			wantStatus: http.StatusBadRequest,
		},
		"invalid player name": {
			method: http.MethodPost, body: `{"players":["Al ice"]}`,
			wantStatus: http.StatusBadRequest,
		},
		"duplicate players": {
			method: http.MethodPost, body: `{"players":["Alice","alice"]}`,
			wantStatus: http.StatusBadRequest,
		},
		"too many players": {
			method:     http.MethodPost,
			body:       `{"players":["p1","p2","p3","p4","p5","p6","p7","p8","p9","p10","p11","p12","p13","p14"]}`,
			wantStatus: http.StatusBadRequest,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			b := newTestBridge(&Config{BridgeAuthDisabled: true})
			req := httptest.NewRequest(test.method, "/api/votes/start", strings.NewReader(test.body))
			rec := httptest.NewRecorder()
			b.handleStart(rec, req)

			if rec.Code != test.wantStatus {
				t.Errorf("status: got %d, want %d (body=%s)", rec.Code, test.wantStatus, rec.Body.String())
			}
			if b.votes.IsOpen() != test.wantOpen {
				t.Errorf("IsOpen after call: got %v, want %v", b.votes.IsOpen(), test.wantOpen)
			}
		})
	}
}

func TestHandleCast(t *testing.T) {
	type setup struct {
		openWith []string // if non-nil, opens voting with these players before the call
		close    bool     // if true, closes voting before the call
	}
	tests := map[string]struct {
		setup      setup
		method     string
		body       string
		wantStatus int
	}{
		"valid cast": {
			setup:  setup{openWith: []string{"Alice"}},
			method: http.MethodPost, body: `{"player":"Alice","username":"v1","value":3}`,
			wantStatus: http.StatusOK,
		},
		"case-insensitive player": {
			setup:  setup{openWith: []string{"Alice"}},
			method: http.MethodPost, body: `{"player":"ALICE","username":"v1","value":3}`,
			wantStatus: http.StatusOK,
		},
		"wrong method": {
			setup:      setup{openWith: []string{"Alice"}},
			method:     http.MethodGet,
			body:       `{"player":"Alice","username":"v1","value":3}`,
			wantStatus: http.StatusMethodNotAllowed,
		},
		"invalid JSON": {
			setup:  setup{openWith: []string{"Alice"}},
			method: http.MethodPost, body: `not json`,
			wantStatus: http.StatusBadRequest,
		},
		"missing player": {
			setup:  setup{openWith: []string{"Alice"}},
			method: http.MethodPost, body: `{"username":"v1","value":3}`,
			wantStatus: http.StatusBadRequest,
		},
		"missing username": {
			setup:  setup{openWith: []string{"Alice"}},
			method: http.MethodPost, body: `{"player":"Alice","value":3}`,
			wantStatus: http.StatusBadRequest,
		},
		"unknown player": {
			setup:  setup{openWith: []string{"Alice"}},
			method: http.MethodPost, body: `{"player":"Bob","username":"v1","value":3}`,
			wantStatus: http.StatusBadRequest,
		},
		"value out of range": {
			setup:  setup{openWith: []string{"Alice"}},
			method: http.MethodPost, body: `{"player":"Alice","username":"v1","value":99}`,
			wantStatus: http.StatusBadRequest,
		},
		"voting closed": {
			setup:  setup{openWith: []string{"Alice"}, close: true},
			method: http.MethodPost, body: `{"player":"Alice","username":"v1","value":3}`,
			wantStatus: http.StatusBadRequest,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			b := newTestBridge(&Config{BridgeAuthDisabled: true})
			if test.setup.openWith != nil {
				b.votes.OpenVoting(test.setup.openWith)
			}
			if test.setup.close {
				b.votes.CloseVoting()
			}
			req := httptest.NewRequest(test.method, "/api/votes/cast", strings.NewReader(test.body))
			rec := httptest.NewRecorder()
			b.handleCast(rec, req)

			if rec.Code != test.wantStatus {
				t.Errorf("status: got %d, want %d (body=%s)", rec.Code, test.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestHandleVotes(t *testing.T) {
	b := newTestBridge(&Config{BridgeAuthDisabled: true})
	b.votes.OpenVoting([]string{"Alice", "Bob"})
	b.votes.CastVote("Alice", "viewer1", 4)

	req := httptest.NewRequest(http.MethodGet, "/api/votes", nil)
	rec := httptest.NewRecorder()
	b.handleVotes(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var got Tally
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Open {
		t.Error("open: got false, want true")
	}
	if len(got.Results) != 2 {
		t.Errorf("results count: got %d, want 2", len(got.Results))
	}

	// Wrong method
	req = httptest.NewRequest(http.MethodPost, "/api/votes", nil)
	rec = httptest.NewRecorder()
	b.handleVotes(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("wrong-method status: got %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleEnd(t *testing.T) {
	b := newTestBridge(&Config{BridgeAuthDisabled: true})
	b.votes.OpenVoting([]string{"Alice"})
	b.votes.CastVote("Alice", "viewer1", 3)

	req := httptest.NewRequest(http.MethodPost, "/api/votes/end", nil)
	rec := httptest.NewRecorder()
	b.handleEnd(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if b.votes.IsOpen() {
		t.Error("IsOpen after end: got true, want false")
	}

	// Wrong method
	req = httptest.NewRequest(http.MethodGet, "/api/votes/end", nil)
	rec = httptest.NewRecorder()
	b.handleEnd(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("wrong-method status: got %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleReset(t *testing.T) {
	b := newTestBridge(&Config{BridgeAuthDisabled: true})
	b.votes.OpenVoting([]string{"Alice"})
	b.votes.CastVote("Alice", "viewer1", 3)

	req := httptest.NewRequest(http.MethodPost, "/api/votes/reset", nil)
	rec := httptest.NewRecorder()
	b.handleReset(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if b.votes.HasVotes() {
		t.Error("HasVotes after reset: got true, want false")
	}
	if !b.votes.IsOpen() {
		t.Error("IsOpen after reset: got false, want true (reset keeps round open)")
	}
}

// TestHandleStart_BodyTooLarge exercises the MaxBytesReader guard — a
// client can't DOS us into parsing a huge body.
func TestHandleStart_BodyTooLarge(t *testing.T) {
	b := newTestBridge(&Config{BridgeAuthDisabled: true})
	big := bytes.Repeat([]byte("a"), maxRequestBodyBytes+1)
	body := bytes.NewReader(big)
	req := httptest.NewRequest(http.MethodPost, "/api/votes/start", body)
	rec := httptest.NewRecorder()
	b.handleStart(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized body status: got %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
