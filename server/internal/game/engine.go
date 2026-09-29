// Package game implements the deterministic authoritative simulation.
//
// The same room configuration and the same ordered input stream must always
// produce the same tick snapshots. Replays re-derive every snapshot from the
// recorded inputs and compare it against the live run, so the server
// conclusion can never be silently rewritten by client-side smoothing.
package game

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

type Command string

const (
	CmdUp    Command = "up"
	CmdDown  Command = "down"
	CmdLeft  Command = "left"
	CmdRight Command = "right"
	CmdWait  Command = "wait"
)

// Input is a command issued by a session for the character it owns.
// Seq is per-session and strictly increasing; duplicates and stale inputs
// carry a seq that has already been observed.
type Input struct {
	SessionID string  `json:"sessionId"`
	CharID    string  `json:"charId"`
	Seq       int64   `json:"seq"`
	Cmd       Command `json:"cmd"`
}

type Char struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Color        string  `json:"color"`
	OwnerSession string  `json:"ownerSession"`
	X            int     `json:"x"`
	Y            int     `json:"y"`
	Facing       Command `json:"facing"`
	// AppliedSeq is the highest input seq the authority has executed for the
	// owning session. It is also the acknowledgement sent back to clients.
	AppliedSeq int64 `json:"appliedSeq"`
}

type Config struct {
	Width     int      `json:"width"`
	Height    int      `json:"height"`
	Obstacles [][2]int `json:"obstacles"`
}

type CharState struct {
	ID         string  `json:"id"`
	X          int     `json:"x"`
	Y          int     `json:"y"`
	Facing     Command `json:"facing"`
	AppliedSeq int64   `json:"appliedSeq"`
}

type Snapshot struct {
	Tick  int64       `json:"tick"`
	Chars []CharState `json:"chars"`
	Hash  string      `json:"hash"`
}

// AppliedInput records that an input was processed at a specific tick.
// Accepted is false when the move was blocked by a wall or another
// character. A duplicate/stale input is not recorded at all.
type AppliedInput struct {
	Tick      int64   `json:"tick"`
	SessionID string  `json:"sessionId"`
	CharID    string  `json:"charId"`
	Seq       int64   `json:"seq"`
	Cmd       Command `json:"cmd"`
	Accepted  bool    `json:"accepted"`
}

// TickRecord is one deterministic append-only event in the room journal.
type TickRecord struct {
	Type   string         `json:"type"` // always "tick"
	Tick   int64          `json:"tick"`
	Inputs []AppliedInput `json:"inputs"`
}

type Room struct {
	ID    string
	Cfg   Config
	Tick  int64
	Chars []*Char // fixed order, keeps iteration deterministic
	byID  map[string]*Char
	walls map[[2]int]struct{}
}

func NewRoom(id string, cfg Config, chars []*Char) *Room {
	r := &Room{ID: id, Cfg: cfg, Chars: chars, byID: map[string]*Char{}, walls: map[[2]int]struct{}{}}
	for _, c := range chars {
		r.byID[c.ID] = c
	}
	for _, o := range cfg.Obstacles {
		r.walls[o] = struct{}{}
	}
	return r
}

// DefaultConfig is used by every freshly created room and by replays.
func DefaultConfig() Config {
	return Config{
		Width:     18,
		Height:    10,
		Obstacles: [][2]int{{5, 2}, {5, 3}, {5, 4}, {12, 5}, {12, 6}, {12, 7}, {8, 7}, {9, 7}},
	}
}

// IsStale reports whether the input duplicates one already processed by the
// owning session. Gaps are allowed; only seq <= AppliedSeq counts as stale.
func (r *Room) IsStale(in Input) bool {
	c := r.byID[in.CharID]
	return c != nil && in.Seq <= c.AppliedSeq
}

// ApplyTick processes one ordered batch of inputs and advances the clock.
// Inputs within the batch are executed in character declaration order and
// then seq order, which is the same order used during replay.
func (r *Room) ApplyTick(inputs []Input) ([]AppliedInput, Snapshot) {
	r.Tick++
	sort.SliceStable(inputs, func(i, j int) bool {
		ti, tj := r.charIndex(inputs[i].CharID), r.charIndex(inputs[j].CharID)
		if ti != tj {
			return ti < tj
		}
		return inputs[i].Seq < inputs[j].Seq
	})
	applied := make([]AppliedInput, 0, len(inputs))
	for _, in := range inputs {
		c := r.byID[in.CharID]
		if c == nil || c.OwnerSession != in.SessionID {
			// Input for an unknown character or from a non-owning session is
			// refused before touching any state.
			continue
		}
		if in.Seq <= c.AppliedSeq {
			// Idempotency: a late duplicate must never execute twice.
			continue
		}
		c.AppliedSeq = in.Seq
		rec := AppliedInput{
			Tick: r.Tick, SessionID: in.SessionID, CharID: in.CharID,
			Seq: in.Seq, Cmd: in.Cmd,
		}
		if in.Cmd != CmdWait {
			c.Facing = in.Cmd
		}
		nx, ny := c.X, c.Y
		switch in.Cmd {
		case CmdUp:
			ny--
		case CmdDown:
			ny++
		case CmdLeft:
			nx--
		case CmdRight:
			nx++
		}
		if r.passable(nx, ny, c.ID) {
			c.X, c.Y = nx, ny
			rec.Accepted = true
		}
		applied = append(applied, rec)
	}
	return applied, r.Snapshot()
}

func (r *Room) charIndex(id string) int {
	for i, c := range r.Chars {
		if c.ID == id {
			return i
		}
	}
	return len(r.Chars)
}

func (r *Room) passable(x, y int, moverID string) bool {
	if x < 0 || y < 0 || x >= r.Cfg.Width || y >= r.Cfg.Height {
		return false
	}
	if _, ok := r.walls[[2]int{x, y}]; ok {
		return false
	}
	for _, c := range r.Chars {
		if c.ID != moverID && c.X == x && c.Y == y {
			return false
		}
	}
	return true
}

// Passable exposes the movement rule so callers (and the matching client
// prediction code) can reason about a proposed move.
func (r *Room) Passable(x, y int, moverID string) bool { return r.passable(x, y, moverID) }

func (r *Room) Snapshot() Snapshot {
	s := Snapshot{Tick: r.Tick, Chars: make([]CharState, 0, len(r.Chars))}
	for _, c := range r.Chars {
		s.Chars = append(s.Chars, CharState{
			ID: c.ID, X: c.X, Y: c.Y, Facing: c.Facing, AppliedSeq: c.AppliedSeq,
		})
	}
	return s
}

// Hash is a canonical content hash of a snapshot, independent of Go map
// iteration order.
func (s Snapshot) HashValue() string {
	clone := Snapshot{Tick: s.Tick, Chars: s.Chars}
	b, _ := json.Marshal(clone)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// Replay rebuilds every snapshot purely from recorded tick records. The room
// must be constructed from the same initial configuration. Returned snapshots
// include recomputed hashes; callers compare them against the live journal.
func Replay(id string, cfg Config, initial []*Char, records []TickRecord) ([]Snapshot, error) {
	r := NewRoom(id, cfg, cloneChars(initial))
	out := make([]Snapshot, 0, len(records))
	for _, rec := range records {
		if rec.Type != "tick" {
			continue
		}
		if rec.Tick != r.Tick+1 {
			return nil, fmt.Errorf("non-contiguous tick in journal: want %d got %d", r.Tick+1, rec.Tick)
		}
		inputs := make([]Input, 0, len(rec.Inputs))
		for _, a := range rec.Inputs {
			inputs = append(inputs, Input{SessionID: a.SessionID, CharID: a.CharID, Seq: a.Seq, Cmd: a.Cmd})
		}
		applied, snap := r.ApplyTick(inputs)
		if len(applied) != len(rec.Inputs) {
			return nil, fmt.Errorf("tick %d replay diverged on input count", rec.Tick)
		}
		for i := range applied {
			if applied[i] != rec.Inputs[i] {
				return nil, fmt.Errorf("tick %d replay diverged on input %d", rec.Tick, i)
			}
		}
		snap.Hash = snap.HashValue()
		out = append(out, snap)
	}
	return out, nil
}

// ReplayRoom applies the journal to a fresh room and returns both the room in
// its final state and every snapshot hash. It is used to resume a room after
// restart without re-emitting ticks that were already journaled.
func ReplayRoom(id string, cfg Config, initial []*Char, records []TickRecord) (*Room, []Snapshot, error) {
	r := NewRoom(id, cfg, cloneChars(initial))
	out := make([]Snapshot, 0, len(records))
	for _, rec := range records {
		if rec.Type != "tick" {
			continue
		}
		if rec.Tick != r.Tick+1 {
			return nil, nil, fmt.Errorf("non-contiguous tick in journal: want %d got %d", r.Tick+1, rec.Tick)
		}
		inputs := make([]Input, 0, len(rec.Inputs))
		for _, a := range rec.Inputs {
			inputs = append(inputs, Input{SessionID: a.SessionID, CharID: a.CharID, Seq: a.Seq, Cmd: a.Cmd})
		}
		applied, snap := r.ApplyTick(inputs)
		if len(applied) != len(rec.Inputs) {
			return nil, nil, fmt.Errorf("tick %d replay diverged on input count", rec.Tick)
		}
		for i := range applied {
			if applied[i] != rec.Inputs[i] {
				return nil, nil, fmt.Errorf("tick %d replay diverged on input %d", rec.Tick, i)
			}
		}
		snap.Hash = snap.HashValue()
		out = append(out, snap)
	}
	return r, out, nil
}

func cloneChars(in []*Char) []*Char {
	out := make([]*Char, len(in))
	for i, c := range in {
		cp := *c
		out[i] = &cp
	}
	return out
}
