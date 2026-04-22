package main

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// VoteState is the canonical vote store. All methods are safe for concurrent use.
type VoteState struct {
	mu      sync.RWMutex
	minVote int
	maxVote int

	open      bool
	players   []string // original casing, insertion order
	startedAt *time.Time
	votes     map[string]map[string]int // lowercase player → (chatter login → value)
}

// PlayerTally holds results for a single contestant.
type PlayerTally struct {
	Player  string         `json:"player"`
	Counts  map[string]int `json:"counts"` // "1".."5" → count
	Total   int            `json:"total"`
	Average *float64       `json:"average"` // nil if no votes cast
}

// Tally is a snapshot of the current vote results, safe to read without a lock.
type Tally struct {
	Open      bool          `json:"open"`
	Players   []string      `json:"players"`
	StartedAt *int64        `json:"startedAt"` // unix ms, omitted if nil
	Results   []PlayerTally `json:"results"`   // sorted by average descending
}

func newVoteState(minVote, maxVote int) *VoteState {
	return &VoteState{
		minVote: minVote,
		maxVote: maxVote,
		votes:   make(map[string]map[string]int),
	}
}

// OpenVoting starts a new voting round with the given contestants.
func (vs *VoteState) OpenVoting(players []string) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	now := time.Now()
	vs.open = true
	// Copy the caller's slice — if they mutate their slice later we'd be reading
	// it under our lock and might see torn state mid-tally.
	vs.players = append([]string(nil), players...)
	vs.startedAt = &now
	vs.votes = make(map[string]map[string]int)
	for _, p := range vs.players {
		vs.votes[strings.ToLower(p)] = make(map[string]int)
	}
}

// CloseVoting ends the round and returns the final tally.
func (vs *VoteState) CloseVoting() Tally {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	vs.open = false
	return vs.computeTally()
}

// ResetVotes clears all votes without opening or closing the round.
func (vs *VoteState) ResetVotes() {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	for key := range vs.votes {
		vs.votes[key] = make(map[string]int)
	}
}

// IsOpen returns whether voting is currently accepting votes.
func (vs *VoteState) IsOpen() bool {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return vs.open
}

// HasVotes returns true if at least one vote has been cast.
func (vs *VoteState) HasVotes() bool {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	for _, playerVotes := range vs.votes {
		if len(playerVotes) > 0 {
			return true
		}
	}
	return false
}

// FindPlayer returns the display name for a player - case-insensitive
func (vs *VoteState) FindPlayer(name string) (display string, ok bool) {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	key := strings.ToLower(name)
	for _, p := range vs.players {
		if strings.ToLower(p) == key {
			return p, true
		}
	}
	return "", false
}

// CastVote records or replaces a vote for the given player by the given chatter.
// Returns the previous value (0 if new) and whether the vote was accepted.
func (vs *VoteState) CastVote(player, username string, value int) (prev int, accepted bool) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if !vs.open || value < vs.minVote || value > vs.maxVote {
		return 0, false
	}
	playerVotes, ok := vs.votes[strings.ToLower(player)]
	if !ok {
		return 0, false
	}
	prev = playerVotes[username]
	playerVotes[username] = value
	return prev, true
}

// GetDetailedVotes returns per-player, per-viewer vote data.
func (vs *VoteState) GetDetailedVotes() map[string]map[string]int {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	out := make(map[string]map[string]int, len(vs.players))
	for _, p := range vs.players {
		src := vs.votes[strings.ToLower(p)]
		cp := make(map[string]int, len(src))
		for k, v := range src {
			cp[k] = v
		}
		out[p] = cp
	}
	return out
}

// GetTally returns a point-in-time snapshot of the current results.
func (vs *VoteState) GetTally() Tally {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return vs.computeTally()
}

// computeTally must be called with at least a read lock held.
func (vs *VoteState) computeTally() Tally {
	results := make([]PlayerTally, 0, len(vs.players))

	for _, player := range vs.players {
		playerVotes := vs.votes[strings.ToLower(player)]

		counts := make(map[string]int, vs.maxVote-vs.minVote+1)
		for v := vs.minVote; v <= vs.maxVote; v++ {
			counts[strconv.Itoa(v)] = 0
		}

		var sum int
		for _, val := range playerVotes {
			counts[strconv.Itoa(val)]++
			sum += val
		}

		total := len(playerVotes)
		var avg *float64
		if total > 0 {
			a := float64(sum) / float64(total)
			avg = &a
		}

		results = append(results, PlayerTally{
			Player:  player,
			Counts:  counts,
			Total:   total,
			Average: avg,
		})
	}

	// Sort by average descending. Players with no votes yet have a nil average and
	// sort to the end — otherwise an unscored player would appear to "win" at 0.0
	// before the first vote came in.
	sort.Slice(results, func(i, j int) bool {
		ai, aj := results[i].Average, results[j].Average
		if ai == nil && aj == nil {
			return false
		}
		if ai == nil {
			return false
		}
		if aj == nil {
			return true
		}
		return *ai > *aj
	})

	var startedAtMs *int64
	if vs.startedAt != nil {
		ms := vs.startedAt.UnixMilli()
		startedAtMs = &ms
	}

	return Tally{
		Open:      vs.open,
		Players:   vs.players,
		StartedAt: startedAtMs,
		Results:   results,
	}
}
