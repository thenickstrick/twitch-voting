package main

import (
	"strconv"
	"sync"
	"testing"
)

func TestVoteState_OpenVoting(t *testing.T) {
	vs := newVoteState(1, 5)
	vs.OpenVoting([]string{"Alice", "Bob"})

	if !vs.IsOpen() {
		t.Error("IsOpen(): got false, want true after OpenVoting")
	}
	if vs.HasVotes() {
		t.Error("HasVotes(): got true, want false on fresh round")
	}
	tally := vs.GetTally()
	if len(tally.Players) != 2 {
		t.Errorf("players: got %d, want 2", len(tally.Players))
	}
	// Starting a new round clears previous votes; exercise that branch.
	vs.CastVote("Alice", "viewer1", 3)
	vs.OpenVoting([]string{"Charlie"})
	if vs.HasVotes() {
		t.Error("HasVotes(): got true, want false after re-opening with new players")
	}
}

func TestVoteState_CastVote(t *testing.T) {
	tests := map[string]struct {
		open         bool // whether voting is open when CastVote runs
		players      []string
		player       string
		username     string
		value        int
		wantAccepted bool
	}{
		"accepts valid vote": {
			open: true, players: []string{"alice"},
			player: "alice", username: "viewer1", value: 3, wantAccepted: true,
		},
		"case-insensitive player lookup": {
			open: true, players: []string{"Alice"},
			player: "ALICE", username: "viewer1", value: 3, wantAccepted: true,
		},
		"rejects when voting closed": {
			open: false, players: []string{"alice"},
			player: "alice", username: "viewer1", value: 3, wantAccepted: false,
		},
		"rejects value below min": {
			open: true, players: []string{"alice"},
			player: "alice", username: "viewer1", value: 0, wantAccepted: false,
		},
		"rejects value above max": {
			open: true, players: []string{"alice"},
			player: "alice", username: "viewer1", value: 6, wantAccepted: false,
		},
		"rejects unknown player": {
			open: true, players: []string{"alice"},
			player: "bob", username: "viewer1", value: 3, wantAccepted: false,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			vs := newVoteState(1, 5)
			vs.OpenVoting(test.players)
			if !test.open {
				vs.CloseVoting()
			}
			_, accepted := vs.CastVote(test.player, test.username, test.value)
			if accepted != test.wantAccepted {
				t.Errorf("accepted: got %v, want %v", accepted, test.wantAccepted)
			}
		})
	}
}

func TestVoteState_VoteReplacement(t *testing.T) {
	vs := newVoteState(1, 5)
	vs.OpenVoting([]string{"alice"})

	prev, accepted := vs.CastVote("alice", "viewer1", 3)
	if !accepted || prev != 0 {
		t.Fatalf("first vote: accepted=%v prev=%d, want accepted=true prev=0", accepted, prev)
	}

	prev, accepted = vs.CastVote("alice", "viewer1", 5)
	if !accepted || prev != 3 {
		t.Errorf("replacement: accepted=%v prev=%d, want accepted=true prev=3", accepted, prev)
	}

	tally := vs.GetTally()
	if tally.Results[0].Total != 1 {
		t.Errorf("total after replacement: got %d, want 1 (replacement, not addition)", tally.Results[0].Total)
	}
	if avg := tally.Results[0].Average; avg == nil || *avg != 5.0 {
		t.Errorf("average after replacement: got %v, want 5.0", avg)
	}
}

// TestVoteState_SortOrder guards the sort comparator: nil averages (no votes
// yet) must never sort above players who have votes — otherwise an unscored
// player appears to "win" before the first vote comes in.
func TestVoteState_SortOrder(t *testing.T) {
	vs := newVoteState(1, 5)
	vs.OpenVoting([]string{"NoVotes", "LowAvg", "HighAvg"})

	vs.CastVote("HighAvg", "viewer1", 5)
	vs.CastVote("LowAvg", "viewer1", 2)

	tally := vs.GetTally()
	wantOrder := []string{"HighAvg", "LowAvg", "NoVotes"}
	if len(tally.Results) != len(wantOrder) {
		t.Fatalf("result count: got %d, want %d", len(tally.Results), len(wantOrder))
	}
	for i, want := range wantOrder {
		if tally.Results[i].Player != want {
			t.Errorf("position %d: got %s, want %s", i, tally.Results[i].Player, want)
		}
	}
	if tally.Results[2].Average != nil {
		t.Errorf("expected nil average at position 2 (NoVotes), got %v", *tally.Results[2].Average)
	}
}

func TestVoteState_ResetVotes(t *testing.T) {
	vs := newVoteState(1, 5)
	vs.OpenVoting([]string{"alice", "bob"})
	vs.CastVote("alice", "viewer1", 3)
	vs.CastVote("bob", "viewer2", 4)

	vs.ResetVotes()

	if !vs.IsOpen() {
		t.Error("IsOpen() after ResetVotes: got false, want true (reset keeps round open)")
	}
	if vs.HasVotes() {
		t.Error("HasVotes() after ResetVotes: got true, want false")
	}
}

// TestVoteState_ConcurrentCasts exercises the mutex — without locks the final
// tally would race and be flaky. Run with -race to make regressions loud.
func TestVoteState_ConcurrentCasts(t *testing.T) {
	vs := newVoteState(1, 5)
	vs.OpenVoting([]string{"alice"})

	const numVoters = 100
	const voteValue = 3

	var wg sync.WaitGroup
	wg.Add(numVoters)
	for i := 0; i < numVoters; i++ {
		go func(i int) {
			defer wg.Done()
			vs.CastVote("alice", "viewer"+strconv.Itoa(i), voteValue)
		}(i)
	}
	wg.Wait()

	tally := vs.GetTally()
	if tally.Results[0].Total != numVoters {
		t.Errorf("total votes: got %d, want %d", tally.Results[0].Total, numVoters)
	}
	if avg := tally.Results[0].Average; avg == nil || *avg != float64(voteValue) {
		t.Errorf("average: got %v, want %v", avg, voteValue)
	}
}

func TestVoteState_FindPlayer(t *testing.T) {
	vs := newVoteState(1, 5)
	vs.OpenVoting([]string{"Alice", "Bob"})

	display, ok := vs.FindPlayer("alice")
	if !ok || display != "Alice" {
		t.Errorf("FindPlayer(alice): got (%q, %v), want (Alice, true)", display, ok)
	}
	display, ok = vs.FindPlayer("BOB")
	if !ok || display != "Bob" {
		t.Errorf("FindPlayer(BOB): got (%q, %v), want (Bob, true)", display, ok)
	}
	if _, ok := vs.FindPlayer("nobody"); ok {
		t.Error("FindPlayer(nobody): got ok=true, want false")
	}
}
