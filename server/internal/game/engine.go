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
	// CmdClaim claims one unit of a shared, finite room resource (a pass
	// slot, a one-time scene item). Several clients may predict the same
	// claim offline; the authority grants it to exactly one input.
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
	// Resource is the shared resource a claim targets (cmd == claim).
	Resource string `json:"resource,omitempty"`
	// DependsOn is the seq of an earlier input from the same session whose
	// success this input relies on (0 = independent). When the dependency
	// was rejected, this input is refused with its own independent reason
	// ("dependency-failed") instead of being silently dropped.
	DependsOn int64 `json:"dependsOn,omitempty"`
	// RuleVersion is the rules version the client based this input on when
	// it was formed (0 = legacy/unversioned). The authority never rewrites
	// it; it decides explicitly which version to apply.
	RuleVersion int `json:"ruleVersion,omitempty"`
}

// ResourceSpec declares one shared finite resource of a room.
type ResourceSpec struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Total int    `json:"total"`
}

// Holder records which character's input was granted a resource unit.
type Holder struct {
	CharID string `json:"charId"`
	Seq    int64  `json:"seq"`
	Tick   int64  `json:"tick"`
}

// ResourceState is the authoritative state of one shared resource. It is
// part of every snapshot (and therefore of the snapshot hash) so a grant
// can never be delivered twice or silently rewritten.
type ResourceState struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Total     int      `json:"total"`
	Remaining int      `json:"remaining"`
	Holders   []Holder `json:"holders"`
}

// Rules is one versioned set of action rules. The authority can switch
// rules mid-session (e.g. temporarily seal a region or change the energy
// cost of an action); every switch increments Version and is journaled so
// replays re-derive identical decisions.
type Rules struct {
	Version int             `json:"version"`
	Sealed  [][2]int        `json:"sealed,omitempty"`
	Costs   map[Command]int `json:"costs"`
	// ClaimsClosed temporarily forbids claiming shared resources. Claims
	// formed under an older version are rejected with needsReconfirm.
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
	// AppliedSeq is the highest input seq the authority has executed for the
	// owning session. It is also the acknowledgement sent back to clients.
	AppliedSeq int64 `json:"appliedSeq"`
}

type Config struct {
	Width     int      `json:"width"`
	Height    int      `json:"height"`
	Obstacles [][2]int `json:"obstacles"`
	// Resources are the shared finite resources clients contend for.
	Resources []ResourceSpec `json:"resources,omitempty"`
}

type CharState struct {
	ID         string  `json:"id"`
	X          int     `json:"x"`
	Y          int     `json:"y"`
	Facing     Command `json:"facing"`
	Energy     int     `json:"energy"`
	AppliedSeq int64   `json:"appliedSeq"`
}

type Snapshot struct {
	Tick      int64           `json:"tick"`
	Chars     []CharState     `json:"chars"`
	Resources []ResourceState `json:"resources,omitempty"`
	Hash      string          `json:"hash"`
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
	Resource  string  `json:"resource,omitempty"`
	DependsOn int64   `json:"dependsOn,omitempty"`
	Accepted  bool    `json:"accepted"`
	X         int     `json:"x"`
	Y         int     `json:"y"`
	// RuleVersion is the basis the client declared on the input;
	// AppliedRuleVersion is the version the authority actually used.
	RuleVersion        int `json:"ruleVersion,omitempty"`
	AppliedRuleVersion int `json:"appliedRuleVersion,omitempty"`
	// Reason explains a rejection: "blocked" | "sealed" |
	// "insufficient-energy" | "resource-exhausted" | "already-claimed" |
	// "unknown-resource" | "dependency-failed".
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
	ID     string
	Cfg    Config
	Tick   int64
	Chars  []*Char // fixed order, keeps iteration deterministic
	Rules  Rules
	byID       map[string]*Char
	walls      map[[2]int]struct{}
	sealed     map[[2]int]struct{}
	resources  map[string]*ResourceState
	resOrder   []string // config declaration order, keeps snapshots deterministic
	outcomes   map[string]map[int64]bool // charId -> seq -> accepted, for dependencies
}

func NewRoom(id string, cfg Config, chars []*Char) *Room {
	r := &Room{
		ID: id, Cfg: cfg, Chars: chars,
		byID: map[string]*Char{}, walls: map[[2]int]struct{}{}, sealed: map[[2]int]struct{}{},
		resources: map[string]*ResourceState{}, outcomes: map[string]map[int64]bool{},
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
	for _, spec := range cfg.Resources {
		total := spec.Total
		if total <= 0 {
			total = 1
		}
		r.resources[spec.ID] = &ResourceState{
			ID: spec.ID, Name: spec.Name, Total: total, Remaining: total, Holders: []Holder{},
		}
		r.resOrder = append(r.resOrder, spec.ID)
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
		Width:     18,
		Height:    10,
		Obstacles: [][2]int{{5, 2}, {5, 3}, {5, 4}, {12, 5}, {12, 6}, {12, 7}, {8, 7}, {9, 7}},
		Resources: []ResourceSpec{
			{ID: "pass", Name: "通行名额", Total: 1},
			{ID: "relic", Name: "场景遗物", Total: 1},
		},
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
			Seq: in.Seq, Cmd: in.Cmd, Resource: in.Resource, DependsOn: in.DependsOn,
			RuleVersion: in.RuleVersion, AppliedRuleVersion: r.Rules.Version,
		}
		if in.Cmd == CmdUp || in.Cmd == CmdDown || in.Cmd == CmdLeft || in.Cmd == CmdRight {
			c.Facing = in.Cmd
		}
		switch {
		case r.dependencyFailed(in):
			// The earlier action this input relied on did not succeed (e.g.
			// another client won the shared resource). Refuse with an
			// independent reason; unrelated inputs are unaffected.
			rec.Reason = "dependency-failed"
		case in.Cmd == CmdClaim:
			r.applyClaim(c, in, &rec)
		default:
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
		if !rec.Accepted && in.RuleVersion > 0 && in.RuleVersion < r.Rules.Version &&
			(rec.Reason == "sealed" || rec.Reason == "insufficient-energy") {
			// Formed under older rules and forbidden by the current ones:
			// the user must re-confirm explicitly under the new rules.
			rec.NeedsReconfirm = true
		}
		rec.X, rec.Y = c.X, c.Y
		applied = append(applied, rec)
		r.recordOutcome(in.CharID, in.Seq, rec.Accepted)
	}
	return applied, r.Snapshot()
}

// dependencyFailed reports whether the input declares a dependency on an
// earlier same-session input whose authoritative outcome was not a success.
func (r *Room) dependencyFailed(in Input) bool {
	if in.DependsOn <= 0 {
		return false
	}
	accepted, known := r.outcomes[in.CharID][in.DependsOn]
	return !known || !accepted
}

func (r *Room) recordOutcome(charID string, seq int64, accepted bool) {
	if r.outcomes[charID] == nil {
		r.outcomes[charID] = map[int64]bool{}
	}
	r.outcomes[charID][seq] = accepted
}

// applyClaim arbitrates one claim on a shared resource. Exactly one input
// can be granted each unit: the winner is decided by deterministic input
// order (character declaration order, then seq), never by arrival time.
func (r *Room) applyClaim(c *Char, in Input, rec *AppliedInput) {
	cost := r.Rules.CostOf(CmdClaim)
	rs, ok := r.resources[in.Resource]
	switch {
	case r.Rules.ClaimsClosed:
		rec.Reason = "sealed"
	case c.Energy < cost:
		rec.Reason = "insufficient-energy"
	case !ok:
		rec.Reason = "unknown-resource"
	case holdsResource(rs, c.ID):
		// One grant per character per resource: a re-issued or duplicated
		// claim can never obtain a second unit.
		rec.Reason = "already-claimed"
	case rs.Remaining <= 0:
		rec.Reason = "resource-exhausted"
	default:
		rs.Remaining--
		rs.Holders = append(rs.Holders, Holder{CharID: c.ID, Seq: in.Seq, Tick: r.Tick})
		c.Energy -= cost
		rec.Accepted = true
	}
}

func holdsResource(rs *ResourceState, charID string) bool {
	for _, h := range rs.Holders {
		if h.CharID == charID {
			return true
		}
	}
	return false
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
			ID: c.ID, X: c.X, Y: c.Y, Facing: c.Facing, Energy: c.Energy, AppliedSeq: c.AppliedSeq,
		})
	}
	for _, id := range r.resOrder {
		rs := r.resources[id]
		holders := make([]Holder, len(rs.Holders))
		copy(holders, rs.Holders)
		s.Resources = append(s.Resources, ResourceState{
			ID: rs.ID, Name: rs.Name, Total: rs.Total, Remaining: rs.Remaining, Holders: holders,
		})
	}
	return s
}

// Hash is a canonical content hash of a snapshot, independent of Go map
// iteration order.
func (s Snapshot) HashValue() string {
	clone := Snapshot{Tick: s.Tick, Chars: s.Chars, Resources: s.Resources}
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
				Resource: a.Resource, DependsOn: a.DependsOn,
				RuleVersion: a.RuleVersion,
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
