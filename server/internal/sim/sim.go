// Package sim implements the deterministic authoritative character simulation.
//
// All positions and velocities use milli-units (1 unit = 1000) as int64 so
// that replaying the same event stream on any machine produces exactly the
// same state and state hash.
package sim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

const (
	Milli       int64 = 1000
	FieldHalf   int64 = 5 * Milli
	MaxSpeed    int64 = 3 * Milli // units per tick
	FrictionNum int64 = 8
	FrictionDen int64 = 10
	KickImpulse int64 = 6 * Milli
)

// CharState is the public, JSON-friendly character snapshot.
type CharState struct {
	ID      string `json:"id"`
	X       int64  `json:"x"`
	Y       int64  `json:"y"`
	VX      int64  `json:"vx"`
	VY      int64  `json:"vy"`
	LastSeq int64  `json:"lastSeq"`
}

// State is the full simulation snapshot at a tick.
type State struct {
	Tick  int64                 `json:"tick"`
	Chars map[string]*CharState `json:"chars"`
}

func NewState(ids []string) *State {
	s := &State{Tick: 0, Chars: map[string]*CharState{}}
	for i, id := range ids {
		offset := int64(i-1) * 2 * Milli
		s.Chars[id] = &CharState{ID: id, X: offset}
	}
	return s
}

func (s *State) Clone() *State {
	c := &State{Tick: s.Tick, Chars: map[string]*CharState{}}
	for id, ch := range s.Chars {
		copy := *ch
		c.Chars[id] = &copy
	}
	return c
}

// Hash is a canonical hash of the state. Replay must reproduce it byte for byte.
func (s *State) Hash() string {
	ids := make([]string, 0, len(s.Chars))
	for id := range s.Chars {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	h := sha256.New()
	fmt.Fprintf(h, "tick=%d", s.Tick)
	for _, id := range ids {
		c := s.Chars[id]
		fmt.Fprintf(h, "|%s:%d,%d,%d,%d,%d", id, c.X, c.Y, c.VX, c.VY, c.LastSeq)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func clamp(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ApplyInput sets the character velocity from a directional input.
// dirX/dirY are each -1, 0 or 1. The call is idempotence-agnostic: callers
// must guarantee each input sequence number is applied at most once.
func (s *State) ApplyInput(id string, seq, dirX, dirY int64) {
	c, ok := s.Chars[id]
	if !ok {
		return
	}
	if seq <= c.LastSeq {
		return // defensive: old input must never apply twice
	}
	c.LastSeq = seq
	c.VX = clamp(dirX, -1, 1) * MaxSpeed
	c.VY = clamp(dirY, -1, 1) * MaxSpeed
}

// ApplyImpulse adds an authoritative server-side velocity impulse (server kick).
func (s *State) ApplyImpulse(id string, dx, dy int64) {
	c, ok := s.Chars[id]
	if !ok {
		return
	}
	c.VX += clamp(dx, -1, 1) * KickImpulse
	c.VY += clamp(dy, -1, 1) * KickImpulse
}

// Advance runs physics for exactly one fixed tick:
// position += velocity, friction, then arena bounds (velocity zeroed at walls).
func (s *State) Advance() {
	for _, c := range s.Chars {
		c.X += c.VX
		c.Y += c.VY
		c.VX = c.VX * FrictionNum / FrictionDen
		c.VY = c.VY * FrictionNum / FrictionDen
		if c.X > FieldHalf {
			c.X = FieldHalf
			c.VX = 0
		}
		if c.X < -FieldHalf {
			c.X = -FieldHalf
			c.VX = 0
		}
		if c.Y > FieldHalf {
			c.Y = FieldHalf
			c.VY = 0
		}
		if c.Y < -FieldHalf {
			c.Y = -FieldHalf
			c.VY = 0
		}
	}
	s.Tick++
}
