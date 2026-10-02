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
	SessionID   string  `json:"sessionId"`
	CharID      string  `json:"charId"`
	Seq         int64   `json:"seq"`
	Cmd         Command `json:"cmd"`
	RuleVersion int     `json:"ruleVersion"`
	Confirmed   bool    `json:"confirmed,omitempty"`
}

// Rules is the authoritative action basis. Clients retain the version used
// when creating an input, while the server decides against the current basis.
type Rules struct {
	Version int            `json:"version"`
	Name    string         `json:"name"`
	Blocked [][2]int       `json:"blocked,omitempty"`
	Costs   map[string]int `json:"costs,omitempty"`
}

type Char struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Color        string  `json:"color"`
	OwnerSession string  `json:"ownerSession"`
	X            int     `json:"x"`
	Y            int     `json:"y"`
	Facing       Command `json:"facing"`
	Energy       int     `json:"energy"`
	// AppliedSeq is the highest input seq the authority has executed for the
	// owning session. It is also the acknowledgement sent back to clients.
	AppliedSeq int64 `json:"appliedSeq"`
	// DecidedSeq is the highest seq with an explicit per-input decision. It
	// can exceed AppliedSeq while a stale-rule input waits for confirmation.
	DecidedSeq int64 `json:"decidedSeq"`
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
	Energy     int     `json:"energy"`
	AppliedSeq int64   `json:"appliedSeq"`
	DecidedSeq int64   `json:"decidedSeq"`
}

type Snapshot struct {
	Tick        int64       `json:"tick"`
	RuleVersion int         `json:"ruleVersion"`
	Rules       Rules       `json:"rules"`
	Chars       []CharState `json:"chars"`
	Hash        string      `json:"hash"`
}

// AppliedInput records that an input was processed at a specific tick.
// Accepted is false when the move was blocked by a wall or another
// character. A duplicate/stale input is not recorded at all.
type AppliedInput struct {
	Tick             int64   `json:"tick"`
	SessionID        string  `json:"sessionId"`
	CharID           string  `json:"charId"`
	Seq              int64   `json:"seq"`
	Cmd              Command `json:"cmd"`
	Accepted         bool    `json:"accepted"`
	Decision         string  `json:"decision,omitempty"`
	Reason           string  `json:"reason,omitempty"`
	RuleVersion      int     `json:"ruleVersion,omitempty"`
	EffectiveVersion int     `json:"effectiveVersion,omitempty"`
	Cost             int     `json:"cost,omitempty"`
	Energy           int     `json:"energy,omitempty"`
	Confirmed        bool    `json:"confirmed,omitempty"`
	X                int     `json:"x"`
	Y                int     `json:"y"`
}

type RuleRecord struct {
	Type  string `json:"type"`
	Tick  int64  `json:"tick"`
	Rules Rules  `json:"rules"`
}

// JournalRecord is either a rule switch or an input tick.
type JournalRecord struct {
	Type   string         `json:"type"`
	Tick   int64          `json:"tick"`
	Rules  Rules          `json:"rules,omitempty"`
	Inputs []AppliedInput `json:"inputs,omitempty"`
}

// TickRecord is one deterministic append-only event in the room journal.
type TickRecord struct {
	Type   string         `json:"type"` // always "tick"
	Tick   int64          `json:"tick"`
	Inputs []AppliedInput `json:"inputs"`
}

type Room struct {
	ID             string
	Cfg            Config
	Tick           int64
	Chars          []*Char // fixed order, keeps iteration deterministic
	byID           map[string]*Char
	walls          map[[2]int]struct{}
	rules          Rules
	pendingConfirm map[string]map[int64]Input
}

func NewRoom(id string, cfg Config, chars []*Char, rules ...Rules) *Room {
	rule := DefaultRulesV1()
	if len(rules) > 0 {
		rule = rules[0]
	}
	r := &Room{ID: id, Cfg: cfg, Chars: chars, byID: map[string]*Char{}, walls: map[[2]int]struct{}{}, rules: rule, pendingConfirm: map[string]map[int64]Input{}}
	for _, c := range chars {
		r.byID[c.ID] = c
		if c.Energy == 0 {
			c.Energy = 10
		}
	}
	r.rebuildWalls()
	return r
}

func DefaultRulesV1() Rules {
	return Rules{Version: 1, Name: "standard", Costs: map[string]int{
		string(CmdUp): 1, string(CmdDown): 1, string(CmdLeft): 1, string(CmdRight): 1, string(CmdWait): 0,
	}}
}

func DefaultRulesV2() Rules {
	rules := DefaultRulesV1()
	rules.Version = 2
	rules.Name = "closed-east-corner-and-expensive-turns"
	rules.Blocked = [][2]int{{3, 2}}
	rules.Costs[string(CmdRight)] = 3
	rules.Costs[string(CmdUp)] = 3
	return rules
}

func RulesForVersion(version int) (Rules, bool) {
	switch version {
	case 1:
		return DefaultRulesV1(), true
	case 2:
		return DefaultRulesV2(), true
	default:
		return Rules{}, false
	}
}

func (r *Room) CurrentRules() Rules { return r.rules }

func (r *Room) PendingConfirm(charID string, seq int64) bool {
	_, ok := r.pendingConfirm[charID][seq]
	return ok
}

// ApplyRules switches the rule basis at the next processing boundary.
func (r *Room) ApplyRules(rules Rules) RuleRecord {
	r.rules = rules
	r.rebuildWalls()
	return RuleRecord{Type: "rule", Tick: r.Tick, Rules: rules}
}

func (r *Room) rebuildWalls() {
	r.walls = map[[2]int]struct{}{}
	for _, obstacle := range r.Cfg.Obstacles {
		r.walls[obstacle] = struct{}{}
	}
	for _, blocked := range r.rules.Blocked {
		r.walls[blocked] = struct{}{}
	}
}

func (rules Rules) cost(cmd Command) int {
	if rules.Costs == nil {
		return 0
	}
	return rules.Costs[string(cmd)]
}

// DefaultConfig is used by every freshly created room and by replays.
func DefaultConfig() Config {
	return Config{
		Width:     18,
		Height:    10,
		Obstacles: [][2]int{{5, 2}, {5, 3}, {5, 4}, {12, 5}, {12, 6}, {12, 7}, {8, 7}, {9, 7}},
	}
}

// IsStale reports whether the input duplicates one already decided by the
// owning session. AppliedSeq is only one kind of terminal decision.
func (r *Room) IsStale(in Input) bool {
	c := r.byID[in.CharID]
	return c != nil && in.Seq <= c.DecidedSeq
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
		if in.Seq <= c.DecidedSeq {
			// Idempotency: a late duplicate must never execute twice.
			continue
		}
		inputVersion := in.RuleVersion
		if inputVersion == 0 {
			inputVersion = 1
		}
		rec := AppliedInput{
			Tick: r.Tick, SessionID: in.SessionID, CharID: in.CharID,
			Seq: in.Seq, Cmd: in.Cmd, RuleVersion: inputVersion,
			EffectiveVersion: r.rules.Version, Confirmed: in.Confirmed,
		}
		if inputVersion != r.rules.Version && !in.Confirmed {
			cost := r.rules.cost(in.Cmd)
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
			if oldRules, ok := RulesForVersion(inputVersion); ok {
				oldCost := oldRules.cost(in.Cmd)
				oldPass := in.Cmd == CmdWait || r.passableWithRules(oldRules, nx, ny, c.ID)
				currentPass := in.Cmd == CmdWait || r.passable(nx, ny, c.ID)
				if oldCost == cost && oldPass == currentPass && cost <= c.Energy {
					goto executeInput
				}
			}
			if cost > c.Energy {
				rec.Accepted = false
				rec.Decision = "rejected"
				rec.Reason = "insufficient-energy-under-current-rules"
				rec.Cost = cost
				rec.Energy = c.Energy
				rec.X, rec.Y = c.X, c.Y
				c.AppliedSeq = in.Seq
				c.DecidedSeq = in.Seq
				applied = append(applied, rec)
				continue
			}
			if in.Cmd != CmdWait && !r.passable(nx, ny, c.ID) {
				rec.Accepted = false
				rec.Decision = "rejected"
				rec.Reason = "blocked-under-current-rules"
				rec.Cost = cost
				rec.Energy = c.Energy
				rec.X, rec.Y = c.X, c.Y
				c.AppliedSeq = in.Seq
				c.DecidedSeq = in.Seq
				applied = append(applied, rec)
				continue
			}
			if r.pendingConfirm[c.ID] == nil {
				r.pendingConfirm[c.ID] = map[int64]Input{}
			}
			pending := in
			pending.RuleVersion = inputVersion
			r.pendingConfirm[c.ID][in.Seq] = pending
			rec.Accepted = false
			rec.Decision = "needs_confirmation"
			rec.Reason = "rule-version-changed"
			rec.Energy = c.Energy
			rec.X, rec.Y = c.X, c.Y
			c.DecidedSeq = in.Seq
			applied = append(applied, rec)
			continue
		}
	executeInput:
		cost := r.rules.cost(in.Cmd)
		if cost > c.Energy {
			rec.Accepted = false
			rec.Decision = "rejected"
			rec.Reason = "insufficient-energy"
			rec.Cost = cost
			rec.Energy = c.Energy
			rec.X, rec.Y = c.X, c.Y
			c.AppliedSeq = in.Seq
			c.DecidedSeq = in.Seq
			applied = append(applied, rec)
			continue
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
			c.Energy -= cost
			rec.Accepted = true
			rec.Decision = "accepted"
		} else {
			rec.Accepted = false
			rec.Decision = "rejected"
			rec.Reason = "blocked"
		}
		rec.Cost = cost
		rec.Energy = c.Energy
		rec.X, rec.Y = c.X, c.Y
		if r.pendingConfirm[c.ID] != nil {
			delete(r.pendingConfirm[c.ID], in.Seq)
		}
		c.AppliedSeq = in.Seq
		c.DecidedSeq = in.Seq
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

func (r *Room) passableWithRules(rules Rules, x, y int, moverID string) bool {
	walls := map[[2]int]struct{}{}
	for _, obstacle := range r.Cfg.Obstacles {
		walls[obstacle] = struct{}{}
	}
	for _, blocked := range rules.Blocked {
		walls[blocked] = struct{}{}
	}
	if x < 0 || y < 0 || x >= r.Cfg.Width || y >= r.Cfg.Height || walls[[2]int{x, y}] != struct{}{} {
		return false
	}
	for _, c := range r.Chars {
		if c.ID != moverID && c.X == x && c.Y == y {
			return false
		}
	}
	return true
}

// ConfirmInput executes a previously withheld stale-rule input after explicit
// user confirmation. Later inputs may already be applied, so confirmation
// does not move AppliedSeq backwards; it appends an independently reasoned
// result to the ordered authority journal.
func (r *Room) ConfirmInput(in Input) (AppliedInput, Snapshot, bool) {
	c := r.byID[in.CharID]
	if c == nil || c.OwnerSession != in.SessionID {
		return AppliedInput{}, Snapshot{}, false
	}
	original, ok := r.pendingConfirm[in.CharID][in.Seq]
	if !ok || original.Cmd != in.Cmd || original.RuleVersion != in.RuleVersion {
		return AppliedInput{}, Snapshot{}, false
	}
	r.Tick++
	confirmed := original
	confirmed.Confirmed = true
	rec := AppliedInput{
		Tick: r.Tick, SessionID: in.SessionID, CharID: in.CharID, Seq: in.Seq, Cmd: original.Cmd,
		RuleVersion: original.RuleVersion, EffectiveVersion: r.rules.Version, Confirmed: true,
	}
	cost := r.rules.cost(original.Cmd)
	if cost > c.Energy {
		rec.Accepted = false
		rec.Decision = "rejected"
		rec.Reason = "insufficient-energy-after-confirmation"
		rec.Cost = cost
		rec.Energy = c.Energy
		rec.X, rec.Y = c.X, c.Y
		delete(r.pendingConfirm[in.CharID], in.Seq)
		return rec, r.Snapshot(), true
	}
	if original.Cmd != CmdWait {
		c.Facing = original.Cmd
	}
	nx, ny := c.X, c.Y
	switch original.Cmd {
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
		c.Energy -= cost
		rec.Accepted = true
		rec.Decision = "accepted"
	} else {
		rec.Accepted = false
		rec.Decision = "rejected"
		rec.Reason = "blocked-after-confirmation"
	}
	rec.Cost = cost
	rec.Energy = c.Energy
	rec.X, rec.Y = c.X, c.Y
	delete(r.pendingConfirm[in.CharID], in.Seq)
	return rec, r.Snapshot(), true
}

func (r *Room) Snapshot() Snapshot {
	s := Snapshot{Tick: r.Tick, RuleVersion: r.rules.Version, Rules: r.rules, Chars: make([]CharState, 0, len(r.Chars))}
	for _, c := range r.Chars {
		s.Chars = append(s.Chars, CharState{
			ID: c.ID, X: c.X, Y: c.Y, Facing: c.Facing, Energy: c.Energy, AppliedSeq: c.AppliedSeq, DecidedSeq: c.DecidedSeq,
		})
	}
	return s
}

// Hash is a canonical content hash of a snapshot, independent of Go map
// iteration order.
func (s Snapshot) HashValue() string {
	clone := Snapshot{Tick: s.Tick, RuleVersion: s.RuleVersion, Rules: s.Rules, Chars: s.Chars}
	b, _ := json.Marshal(clone)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// ReplayJournal rebuilds state from rule switches and input ticks in the
// exact append order in which the authority persisted them.
func ReplayJournal(id string, cfg Config, initial []*Char, records []JournalRecord) ([]Snapshot, error) {
	_, snaps, err := replayJournal(id, cfg, initial, records)
	return snaps, err
}

func replayJournal(id string, cfg Config, initial []*Char, records []JournalRecord) (*Room, []Snapshot, error) {
	r := NewRoom(id, cfg, cloneChars(initial))
	out := make([]Snapshot, 0)
	for _, rec := range records {
		switch rec.Type {
		case "rule":
			if rec.Tick != r.Tick {
				return nil, nil, fmt.Errorf("rule event at tick %d applied at %d", rec.Tick, r.Tick)
			}
			r.ApplyRules(rec.Rules)
		case "tick":
			if rec.Tick != r.Tick+1 {
				return nil, nil, fmt.Errorf("non-contiguous tick in journal: want %d got %d", r.Tick+1, rec.Tick)
			}
			allConfirmed := len(rec.Inputs) > 0
			if len(rec.Inputs) > 0 {
				for _, applied := range rec.Inputs {
					if !applied.Confirmed {
						allConfirmed = false
					}
				}
			}
			if allConfirmed {
				var snap Snapshot
				for _, applied := range rec.Inputs {
					in := Input{SessionID: applied.SessionID, CharID: applied.CharID, Seq: applied.Seq, Cmd: applied.Cmd, RuleVersion: applied.RuleVersion, Confirmed: true}
					got, nextSnap, ok := r.ConfirmInput(in)
					if !ok || got != applied {
						return nil, nil, fmt.Errorf("tick %d confirmation replay diverged", rec.Tick)
					}
					snap = nextSnap
				}
				snap.Hash = snap.HashValue()
				out = append(out, snap)
				continue
			}
			inputs := make([]Input, 0, len(rec.Inputs))
			for _, applied := range rec.Inputs {
				inputs = append(inputs, Input{SessionID: applied.SessionID, CharID: applied.CharID, Seq: applied.Seq, Cmd: applied.Cmd, RuleVersion: applied.RuleVersion, Confirmed: applied.Confirmed})
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
	}
	return r, out, nil
}

// ReplayRoomJournal is the resume variant and returns the restored room.
func ReplayRoomJournal(id string, cfg Config, initial []*Char, records []JournalRecord) (*Room, []Snapshot, error) {
	r, snaps, err := replayJournal(id, cfg, initial, records)
	if err != nil {
		return nil, nil, err
	}
	return r, snaps, nil
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
