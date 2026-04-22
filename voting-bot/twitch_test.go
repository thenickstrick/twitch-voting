package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestValidatePlayerNames(t *testing.T) {
	tests := map[string]struct {
		players []string
		wantErr bool
	}{
		"single valid":               {[]string{"Alice"}, false},
		"multiple valid":             {[]string{"Alice", "Bob", "Charlie"}, false},
		"with underscores":           {[]string{"Player_1", "Player_2"}, false},
		"digits":                     {[]string{"robloxer123"}, false},
		"exactly at max length":      {[]string{strings.Repeat("a", maxPlayerName)}, false},
		"over max length":            {[]string{strings.Repeat("a", maxPlayerName+1)}, true},
		"empty string":               {[]string{""}, true},
		"contains space":             {[]string{"Al ice"}, true},
		"contains dash":              {[]string{"Al-ice"}, true},
		"contains special":           {[]string{"Al!ice"}, true},
		"exact duplicate":            {[]string{"Alice", "Alice"}, true},
		"case-insensitive duplicate": {[]string{"Alice", "ALICE"}, true},
		"one valid one invalid":      {[]string{"Alice", "Bob!"}, true},
		"unicode letter":             {[]string{"Álice"}, true}, // regex \w in Go is ASCII only
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := validatePlayerNames(test.players)
			if (err != nil) != test.wantErr {
				t.Errorf("validatePlayerNames(%v): err=%v, wantErr=%v", test.players, err, test.wantErr)
			}
		})
	}
}

// TestIsValidReconnectURL guards the EventSub reconnect-URL allowlist. A
// malicious Twitch response could ship an attacker-controlled URL and our
// reader would trust it unless this check holds.
func TestIsValidReconnectURL(t *testing.T) {
	tests := map[string]struct {
		url  string
		want bool
	}{
		"canonical eventsub": {"wss://eventsub.wss.twitch.tv/ws?session=abc", true},
		"canonical root":     {"wss://eventsub.wss.twitch.tv/", true},
		"ws instead of wss":  {"ws://eventsub.wss.twitch.tv/ws", false},
		"https scheme":       {"https://eventsub.wss.twitch.tv/ws", false},
		"wrong host":         {"wss://evil.com/ws", false},
		"host suffix attack": {"wss://eventsub.wss.twitch.tv.evil.com/ws", false},
		"host prefix attack": {"wss://evil.eventsub.wss.twitch.tv/ws", false},
		"empty string":       {"", false},
		"non-url garbage":    {"not a url at all! ---", false},
		"missing scheme":     {"eventsub.wss.twitch.tv/ws", false},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := isValidReconnectURL(test.url); got != test.want {
				t.Errorf("isValidReconnectURL(%q): got %v, want %v", test.url, got, test.want)
			}
		})
	}
}

func TestVoteRe(t *testing.T) {
	tests := map[string]struct {
		input      string
		wantMatch  bool
		wantPlayer string
		wantValue  string
	}{
		"simple":             {"!vote Alice 3", true, "Alice", "3"},
		"uppercase command":  {"!VOTE Alice 3", true, "Alice", "3"},
		"mixed case command": {"!VoTe Alice 3", true, "Alice", "3"},
		"multi-space":        {"!vote  Alice   3", true, "Alice", "3"},
		"tab-separated":      {"!vote\tAlice\t3", true, "Alice", "3"},
		"leading whitespace": {" !vote Alice 3", false, "", ""},
		"trailing content":   {"!vote Alice 3 extra", false, "", ""},
		"no value":           {"!vote Alice", false, "", ""},
		"non-numeric value":  {"!vote Alice three", false, "", ""},
		"missing player":     {"!vote 3", false, "", ""},
		"unrelated message":  {"hi chat", false, "", ""},
		"partial prefix":     {"!vot Alice 3", false, "", ""},
		"negative value":     {"!vote Alice -3", false, "", ""},
		"decimal value":      {"!vote Alice 3.5", false, "", ""},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			m := voteRe.FindStringSubmatch(test.input)
			if (m != nil) != test.wantMatch {
				t.Fatalf("match: got %v, want %v (submatch=%v)", m != nil, test.wantMatch, m)
			}
			if !test.wantMatch {
				return
			}
			if m[1] != test.wantPlayer {
				t.Errorf("player: got %q, want %q", m[1], test.wantPlayer)
			}
			if m[2] != test.wantValue {
				t.Errorf("value: got %q, want %q", m[2], test.wantValue)
			}
		})
	}
}

func TestTruncateMessage(t *testing.T) {
	// Twitch limits on character count (runes), not bytes. A message of 500
	// multi-byte runes is legal even though it's ~2000 bytes. These cases
	// exercise both the byte-vs-rune distinction and the boundary conditions.
	tests := map[string]struct {
		input      string
		wantRunes  int
		wantSuffix string // "" = no truncation marker expected
	}{
		"short ASCII": {
			input: "hello", wantRunes: 5, wantSuffix: "",
		},
		"exactly at limit (ASCII)": {
			input:     strings.Repeat("a", twitchMaxMessageLen),
			wantRunes: twitchMaxMessageLen, wantSuffix: "",
		},
		"just over limit (ASCII)": {
			input:     strings.Repeat("a", twitchMaxMessageLen+1),
			wantRunes: twitchMaxMessageLen, wantSuffix: "…",
		},
		"exactly at limit (multi-byte)": {
			input:     strings.Repeat("🔥", twitchMaxMessageLen),
			wantRunes: twitchMaxMessageLen, wantSuffix: "",
		},
		"over limit (multi-byte)": {
			input:     strings.Repeat("🔥", twitchMaxMessageLen+1),
			wantRunes: twitchMaxMessageLen, wantSuffix: "…",
		},
		"mixed ASCII and emoji under limit": {
			input:     "hello 🔥",
			wantRunes: 7, wantSuffix: "",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got := truncateMessage(test.input)
			if n := utf8.RuneCountInString(got); n != test.wantRunes {
				t.Errorf("rune count: got %d, want %d", n, test.wantRunes)
			}
			if test.wantSuffix != "" && !strings.HasSuffix(got, test.wantSuffix) {
				t.Errorf("expected suffix %q, got %q", test.wantSuffix, got)
			}
			if test.wantSuffix == "" && got != test.input {
				t.Errorf("expected passthrough, got different output")
			}
		})
	}
}

func TestChatMessageEvent_IsModerator(t *testing.T) {
	// Construct the anonymous badge struct inline — it's awkward but avoids
	// leaking internals into the production types.
	type badge = struct {
		SetID string `json:"set_id"`
	}
	tests := map[string]struct {
		badges []badge
		want   bool
	}{
		"no badges":                       {nil, false},
		"moderator only":                  {[]badge{{SetID: "moderator"}}, true},
		"vip only":                        {[]badge{{SetID: "vip"}}, false},
		"subscriber only":                 {[]badge{{SetID: "subscriber"}}, false},
		"mod among many":                  {[]badge{{SetID: "subscriber"}, {SetID: "moderator"}}, true},
		"broadcaster badge alone not mod": {[]badge{{SetID: "broadcaster"}}, false},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			ev := chatMessageEvent{Badges: test.badges}
			if got := ev.isModerator(); got != test.want {
				t.Errorf("isModerator: got %v, want %v", got, test.want)
			}
		})
	}
}
