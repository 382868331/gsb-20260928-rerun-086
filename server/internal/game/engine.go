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

// RefusedInput records an input the authority deliberately refused instead
// of executing. A refused seq still advances AppliedSeq: it is a final
// authority decision, so a later redelivery is stale and can never mutate
// state a second time.
//
// Reasons:
//   - "offline-overflow": more than MaxAdmittedPerTick contiguous inputs of
//     one session reached the same tick (the tail of an offline burst);
//   - "offline-window": the seq jumped ahead of the still-unacked window and
//     the missing prefix can no longer be waited for.
type RefusedInput struct {
	Tick      int64  `json:"tick"`
	SessionID string `json:"sessionId"`
	CharID    string `json:"charId"`
	Seq       int64  `json:"seq"`
	Reason    string `json:"reason"`
}

const (
	// MaxAdmittedPerTick bounds how many contiguous inputs of one session are
	// executed in a single authority tick. Everything contiguous beyond it is
	// refused as an offline burst so the authority never silently replays an
	// unbounded client backlog.
	MaxAdmittedPerTick = 4
	// MaxPendingGap bounds how far ahead of AppliedSeq an out-of-order input
	// may be while waiting for its missing prefix. Beyond it the input is
	// refused instead of stalling the session forever.
	MaxPendingGap = 8
)

// TickRecord is one deterministic append-only event in the room journal.
type TickRecord struct {
	Type    string         `json:"type"` // always "tick"
	Tick    int64          `json:"tick"`
	Inputs  []AppliedInput `json:"inputs"`
	Refused []RefusedInput `json:"refused,omitempty"`
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
// Inputs are executed in character declaration order and then seq order, the
// same order used during replay.
//
// Besides executed inputs it returns:
//   - refused: final rejections (overflow / gap window), already acked;
//   - deferred: inputs that arrived ahead of a missing seq prefix; the
//     caller must feed them back into a later tick, their relative order and
//     identity preserved.
func (r *Room) ApplyTick(inputs []Input) (applied []AppliedInput, refused []RefusedInput, deferred []Input, snap Snapshot) {
	r.Tick++
	applied = make([]AppliedInput, 0, len(inputs))
	refused = make([]RefusedInput, 0)
	deferred = make([]Input, 0)
	for _, c := range r.Chars {
		mine := make([]Input, 0, len(inputs))
		seen := map[int64]struct{}{}
		for _, in := range inputs {
			if in.CharID != c.ID || in.SessionID != c.OwnerSession {
				continue
			}
			if in.Seq <= c.AppliedSeq {
				// Idempotency: a confirmed-old or duplicated late input is
				// dropped before touching any state.
				continue
			}
			if _, dup := seen[in.Seq]; dup {
				continue
			}
			seen[in.Seq] = struct{}{}
			mine = append(mine, in)
		}
		sort.SliceStable(mine, func(i, j int) bool { return mine[i].Seq < mine[j].Seq })

		budget := MaxAdmittedPerTick
		for idx := 0; idx < len(mine); {
			in := mine[idx]
			expected := c.AppliedSeq + 1
			switch {
			case in.Seq == expected && budget > 0:
				// Contiguous next input and admission budget left: execute.
				applied = append(applied, r.execute(c, in))
				c.AppliedSeq = in.Seq
				budget--
				idx++
			case in.Seq == expected:
				// Contiguous but the offline burst exceeds the per-tick
				// admission budget. The whole contiguous tail is refused and
				// acked; it can never execute later.
				c.AppliedSeq = in.Seq
				refused = append(refused, RefusedInput{
					Tick: r.Tick, SessionID: in.SessionID, CharID: in.CharID,
					Seq: in.Seq, Reason: "offline-overflow",
				})
				idx++
			case in.Seq-c.AppliedSeq > MaxPendingGap:
				// Jumped beyond the unacked window; refusing is final and
				// keeps the session from stalling on a lost prefix.
				c.AppliedSeq = in.Seq
				refused = append(refused, RefusedInput{
					Tick: r.Tick, SessionID: in.SessionID, CharID: in.CharID,
					Seq: in.Seq, Reason: "offline-window",
				})
				idx++
			default:
				// Gap: an earlier seq is still in flight (delay/reorder). Keep
				// identity and order, retry on a later tick.
				deferred = append(deferred, mine[idx:]...)
				idx = len(mine)
			}
		}
	}
	return applied, refused, deferred, r.Snapshot()
}

// execute applies one admitted input to its character and reports whether it
// actually moved (a wall/collision still consumes the seq).
func (r *Room) execute(c *Char, in Input) AppliedInput {
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
	return rec
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
		inputs := recordInputs(rec)
		applied, refused, deferred, snap := r.ApplyTick(inputs)
		if err := compareTick(rec, applied, refused, deferred); err != nil {
			return nil, fmt.Errorf("tick %d %w", rec.Tick, err)
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
		inputs := recordInputs(rec)
		applied, refused, deferred, snap := r.ApplyTick(inputs)
		if err := compareTick(rec, applied, refused, deferred); err != nil {
			return nil, nil, fmt.Errorf("tick %d %w", rec.Tick, err)
		}
		snap.Hash = snap.HashValue()
		out = append(out, snap)
	}
	return r, out, nil
}

// recordInputs rebuilds the final input set of a tick (executed and refused)
// so replay drives the engine through exactly the same decisions.
func recordInputs(rec TickRecord) []Input {
	inputs := make([]Input, 0, len(rec.Inputs)+len(rec.Refused))
	for _, a := range rec.Inputs {
		inputs = append(inputs, Input{SessionID: a.SessionID, CharID: a.CharID, Seq: a.Seq, Cmd: a.Cmd})
	}
	// Refused records carry no command in the journal (the command was never
	// executed); CmdWait is a neutral placeholder that only advances the ack.
	for _, f := range rec.Refused {
		inputs = append(inputs, Input{SessionID: f.SessionID, CharID: f.CharID, Seq: f.Seq, Cmd: CmdWait})
	}
	return inputs
}

func compareTick(rec TickRecord, applied []AppliedInput, refused []RefusedInput, deferred []Input) error {
	if len(deferred) != 0 {
		return fmt.Errorf("replay left inputs deferred")
	}
	if len(applied) != len(rec.Inputs) {
		return fmt.Errorf("replay diverged on applied count: %d vs %d", len(applied), len(rec.Inputs))
	}
	for i := range applied {
		if applied[i] != rec.Inputs[i] {
			return fmt.Errorf("replay diverged on applied input %d", i)
		}
	}
	if len(refused) != len(rec.Refused) {
		return fmt.Errorf("replay diverged on refused count: %d vs %d", len(refused), len(rec.Refused))
	}
	for i := range refused {
		if refused[i] != rec.Refused[i] {
			return fmt.Errorf("replay diverged on refused input %d", i)
		}
	}
	return nil
}

func cloneChars(in []*Char) []*Char {
	out := make([]*Char, len(in))
	for i, c := range in {
		cp := *c
		out[i] = &cp
	}
	return out
}
