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
	// CmdClaim attempts to take one slot of the room's finite shared
	// resource (a pass that only a limited number of characters can hold).
	CmdClaim Command = "claim"
)

// Input is a command issued by a session for the character it owns.
// Seq is per-session and strictly increasing; duplicates and stale inputs
// carry a seq that has already been observed.
type Input struct {
	SessionID string  `json:"sessionId"`
	CharID    string  `json:"charId"`
	Seq       int64   `json:"seq"`
	Cmd       Command `json:"cmd"`
	// RuleVersion is the rules version the client based this input on when
	// it was formed (0 = legacy/unversioned). The authority never rewrites
	// it; it decides explicitly which version to apply.
	RuleVersion int `json:"ruleVersion,omitempty"`
	// DependsOn is the seq of an earlier input of the SAME session whose
	// success this input relies on (0 = independent). If that input was
	// rejected, this one is rejected too — with its own explicit reason —
	// while independent inputs keep processing normally.
	DependsOn int64 `json:"dependsOn,omitempty"`
}

// Rules is one versioned set of action rules. The authority can switch
// rules mid-session (e.g. temporarily seal a region or change the energy
// cost of an action); every switch increments Version and is journaled so
// replays re-derive identical decisions.
type Rules struct {
	Version int             `json:"version"`
	Sealed  [][2]int        `json:"sealed,omitempty"`
	Costs   map[Command]int `json:"costs"`
	// ClaimsClosed forbids shared-resource claims under this version.
	// Inputs formed under an older version are not silently rewritten:
	// they are rejected with an explicit re-confirm request.
	ClaimsClosed bool `json:"claimsClosed,omitempty"`
}

// DefaultRules is version 1: nothing sealed, every move costs 1 energy.
func DefaultRules() Rules {
	return Rules{
		Version: 1,
		Costs: map[Command]int{
			CmdUp: 1, CmdDown: 1, CmdLeft: 1, CmdRight: 1, CmdWait: 0, CmdClaim: 0,
		},
	}
}

// CostOf returns the energy cost of a command under these rules.
func (r Rules) CostOf(cmd Command) int {
	if c, ok := r.Costs[cmd]; ok {
		return c
	}
	if cmd == CmdWait {
		return 0
	}
	return 1
}

// RulesRecord is the journaled rule-switch event. It takes effect at Tick,
// before that tick's inputs are executed.
type RulesRecord struct {
	Type  string `json:"type"` // always "rules"
	Tick  int64  `json:"tick"`
	Rules Rules  `json:"rules"`
}

const (
	maxEnergy     = 100
	energyPerTick = 2
)

type Char struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Color        string  `json:"color"`
	OwnerSession string  `json:"ownerSession"`
	X            int     `json:"x"`
	Y            int     `json:"y"`
	Facing       Command `json:"facing"`
	Energy       int     `json:"energy"`
	// Passes is the number of shared-resource slots this character holds.
	Passes int `json:"passes"`
	// AppliedSeq is the highest input seq the authority has executed for the
	// owning session. It is also the acknowledgement sent back to clients.
	AppliedSeq int64 `json:"appliedSeq"`
}

type Config struct {
	Width     int      `json:"width"`
	Height    int      `json:"height"`
	Obstacles [][2]int `json:"obstacles"`
	// Passes is the total number of shared-resource slots ("通行名额")
	// all characters in the room contend for.
	Passes int `json:"passes"`
}

type CharState struct {
	ID         string  `json:"id"`
	X          int     `json:"x"`
	Y          int     `json:"y"`
	Facing     Command `json:"facing"`
	Energy     int     `json:"energy"`
	Passes     int     `json:"passes"`
	AppliedSeq int64   `json:"appliedSeq"`
}

type Snapshot struct {
	Tick  int64       `json:"tick"`
	Chars []CharState `json:"chars"`
	// PassesLeft is the remaining shared-resource pool. It is part of the
	// authoritative state (and its hash) so every client folds the same
	// adjudication instead of the last prediction that arrived.
	PassesLeft int    `json:"passesLeft"`
	Hash       string `json:"hash"`
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
	X         int     `json:"x"`
	Y         int     `json:"y"`
	// RuleVersion is the basis the client declared on the input;
	// AppliedRuleVersion is the version the authority actually used.
	RuleVersion        int `json:"ruleVersion,omitempty"`
	AppliedRuleVersion int `json:"appliedRuleVersion,omitempty"`
	// DependsOn mirrors the dependency declared on the input.
	DependsOn int64 `json:"dependsOn,omitempty"`
	// Reason explains a rejection: "blocked" | "sealed" |
	// "insufficient-energy" | "resource-exhausted" | "dependency-rejected" |
	// "claims-closed".
	Reason string `json:"reason,omitempty"`
	// NeedsReconfirm is set when an input formed under older rules was
	// rejected because the current rules forbid it; the user must
	// explicitly re-confirm it under the new rules instead of the
	// authority silently rewriting the original input.
	NeedsReconfirm bool `json:"needsReconfirm,omitempty"`
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
	Rules Rules
	// PassesLeft is the remaining shared-resource pool. Grants happen only
	// inside ApplyTick, so a duplicate delivery or a late confirmation can
	// never grant the same slot twice.
	PassesLeft int
	byID       map[string]*Char
	walls      map[[2]int]struct{}
	sealed     map[[2]int]struct{}
	// outcomes records per-character accept/reject results of every applied
	// input so that dependent inputs can be adjudicated. It is rebuilt
	// deterministically by replay (same inputs -> same outcomes).
	outcomes map[string]map[int64]bool
}

func NewRoom(id string, cfg Config, chars []*Char) *Room {
	r := &Room{
		ID: id, Cfg: cfg, Chars: chars, PassesLeft: cfg.Passes,
		byID: map[string]*Char{}, walls: map[[2]int]struct{}{},
		sealed: map[[2]int]struct{}{}, outcomes: map[string]map[int64]bool{},
	}
	for _, c := range chars {
		r.byID[c.ID] = c
		if c.Energy == 0 {
			c.Energy = maxEnergy
		}
	}
	for _, o := range cfg.Obstacles {
		r.walls[o] = struct{}{}
	}
	r.SetRules(DefaultRules())
	return r
}

// SetRules switches the active rules. It is only called at a tick boundary
// and the switch is journaled as a RulesRecord so replay is deterministic.
func (r *Room) SetRules(rules Rules) {
	if rules.Costs == nil {
		rules.Costs = DefaultRules().Costs
	}
	r.Rules = rules
	r.sealed = map[[2]int]struct{}{}
	for _, s := range rules.Sealed {
		r.sealed[s] = struct{}{}
	}
}

// DefaultConfig is used by every freshly created room and by replays.
func DefaultConfig() Config {
	return Config{
		Width:  18,
		Height: 10,
		// A single shared pass slot: exactly one character can hold it, so
		// concurrent claims from both clients always produce a conflict.
		Passes:    1,
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
	for _, c := range r.Chars {
		// Deterministic energy regen, part of the tick so replay matches.
		c.Energy += energyPerTick
		if c.Energy > maxEnergy {
			c.Energy = maxEnergy
		}
	}
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
			RuleVersion: in.RuleVersion, AppliedRuleVersion: r.Rules.Version,
			DependsOn: in.DependsOn,
		}
		// A dependency on a rejected (or never executed) earlier action of
		// the same session fails this input with its own explicit reason;
		// unrelated inputs of the same session are unaffected.
		depFailed := false
		if in.DependsOn > 0 {
			ok, known := r.outcomes[in.CharID][in.DependsOn]
			depFailed = !known || !ok
		}
		switch {
		case depFailed:
			rec.Reason = "dependency-rejected"
		case in.Cmd == CmdClaim:
			r.adjudicateClaim(c, in, &rec)
		default:
			r.adjudicateMove(c, in, &rec)
		}
		if !rec.Accepted && in.RuleVersion > 0 && in.RuleVersion < r.Rules.Version &&
			(rec.Reason == "sealed" || rec.Reason == "insufficient-energy" || rec.Reason == "claims-closed") {
			// Formed under older rules and forbidden by the current ones:
			// the user must re-confirm explicitly under the new rules.
			rec.NeedsReconfirm = true
		}
		rec.X, rec.Y = c.X, c.Y
		applied = append(applied, rec)
		if r.outcomes[in.CharID] == nil {
			r.outcomes[in.CharID] = map[int64]bool{}
		}
		r.outcomes[in.CharID][in.Seq] = rec.Accepted
	}
	return applied, r.Snapshot()
}

// adjudicateClaim resolves a shared-resource claim. Within one tick inputs
// run in (character declaration order, seq) order, so the first valid claim
// wins the slot and every later claim — same tick or after exhaustion — is
// rejected with an explicit reason. Arrival order plays no part here.
func (r *Room) adjudicateClaim(c *Char, in Input, rec *AppliedInput) {
	cost := r.Rules.CostOf(in.Cmd)
	switch {
	case r.Rules.ClaimsClosed:
		rec.Reason = "claims-closed"
	case c.Energy < cost:
		rec.Reason = "insufficient-energy"
	case r.PassesLeft <= 0:
		rec.Reason = "resource-exhausted"
	default:
		r.PassesLeft--
		c.Passes++
		c.Energy -= cost
		rec.Accepted = true
	}
}

// adjudicateMove resolves a movement/wait command against bounds, walls,
// sealed cells, collisions and energy costs.
func (r *Room) adjudicateMove(c *Char, in Input, rec *AppliedInput) {
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
	cost := r.Rules.CostOf(in.Cmd)
	switch {
	case !r.passable(nx, ny, c.ID):
		rec.Reason = "blocked"
	case r.isSealed(nx, ny):
		rec.Reason = "sealed"
	case c.Energy < cost:
		rec.Reason = "insufficient-energy"
	default:
		c.X, c.Y = nx, ny
		c.Energy -= cost
		rec.Accepted = true
	}
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

// isSealed reports whether the current rules temporarily close the cell.
func (r *Room) isSealed(x, y int) bool {
	_, ok := r.sealed[[2]int{x, y}]
	return ok
}

// Passable exposes the movement rule so callers (and the matching client
// prediction code) can reason about a proposed move.
func (r *Room) Passable(x, y int, moverID string) bool { return r.passable(x, y, moverID) }

func (r *Room) Snapshot() Snapshot {
	s := Snapshot{Tick: r.Tick, Chars: make([]CharState, 0, len(r.Chars))}
	for _, c := range r.Chars {
		s.Chars = append(s.Chars, CharState{
			ID: c.ID, X: c.X, Y: c.Y, Facing: c.Facing, Energy: c.Energy,
			Passes: c.Passes, AppliedSeq: c.AppliedSeq,
		})
	}
	s.PassesLeft = r.PassesLeft
	return s
}

// Hash is a canonical content hash of a snapshot, independent of Go map
// iteration order.
func (s Snapshot) HashValue() string {
	clone := Snapshot{Tick: s.Tick, Chars: s.Chars, PassesLeft: s.PassesLeft}
	b, _ := json.Marshal(clone)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// JournalEvent is one ordered journal event: either a tick batch or a rule
// switch. Replay consumes events in journal order so a rule change takes
// effect at exactly the same tick as it did live.
type JournalEvent struct {
	Tick  *TickRecord
	Rules *RulesRecord
}

// ReplayJournal rebuilds every snapshot purely from recorded events. The
// room must be constructed from the same initial configuration. Returned
// snapshots include recomputed hashes; callers compare them against the
// live journal.
func ReplayJournal(id string, cfg Config, initial []*Char, events []JournalEvent) (*Room, []Snapshot, error) {
	r := NewRoom(id, cfg, cloneChars(initial))
	out := make([]Snapshot, 0, len(events))
	for _, ev := range events {
		if ev.Rules != nil {
			if ev.Rules.Tick != r.Tick+1 {
				return nil, nil, fmt.Errorf("rule switch at tick %d, want %d", ev.Rules.Tick, r.Tick+1)
			}
			r.SetRules(ev.Rules.Rules)
			continue
		}
		rec := ev.Tick
		if rec == nil {
			continue
		}
		if rec.Tick != r.Tick+1 {
			return nil, nil, fmt.Errorf("non-contiguous tick in journal: want %d got %d", r.Tick+1, rec.Tick)
		}
		inputs := make([]Input, 0, len(rec.Inputs))
		for _, a := range rec.Inputs {
			inputs = append(inputs, Input{
				SessionID: a.SessionID, CharID: a.CharID, Seq: a.Seq, Cmd: a.Cmd,
				RuleVersion: a.RuleVersion, DependsOn: a.DependsOn,
			})
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

// Replay rebuilds snapshots from tick records only (no rule switches).
func Replay(id string, cfg Config, initial []*Char, records []TickRecord) ([]Snapshot, error) {
	events := make([]JournalEvent, 0, len(records))
	for i := range records {
		if records[i].Type != "tick" {
			continue
		}
		events = append(events, JournalEvent{Tick: &records[i]})
	}
	_, snaps, err := ReplayJournal(id, cfg, initial, events)
	return snaps, err
}

// ReplayRoom applies the journal to a fresh room and returns both the room
// in its final state and every snapshot hash. It is used to resume a room
// after restart without re-emitting ticks that were already journaled.
func ReplayRoom(id string, cfg Config, initial []*Char, records []TickRecord) (*Room, []Snapshot, error) {
	events := make([]JournalEvent, 0, len(records))
	for i := range records {
		if records[i].Type != "tick" {
			continue
		}
		events = append(events, JournalEvent{Tick: &records[i]})
	}
	return ReplayJournal(id, cfg, initial, events)
}

func cloneChars(in []*Char) []*Char {
	out := make([]*Char, len(in))
	for i, c := range in {
		cp := *c
		out[i] = &cp
	}
	return out
}
