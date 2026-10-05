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

// Contention-round tuning. A round is the persistent adjudication
// identity for one wave of competing claims on the same resource:
//
//   - it opens when the first claim of the wave executes (OpenTick);
//   - its collection boundary is explicit: claims executing in ticks
//     [OpenTick, CloseTick) join the round, where CloseTick =
//     OpenTick + RoundCollectTicks;
//   - at CloseTick every collected claim is adjudicated in a stable
//     order (character declaration order, then seq) that does not
//     depend on how the claims were batched by the network;
//   - if a client's known input is still missing at the boundary, the
//     round waits (RoundWaitExtend ticks per postponement, at most
//     MaxRoundPostponements) and then closes anyway, explicitly
//     cancelling the missing claimant instead of silently letting
//     arrival order decide.
const (
	RoundCollectTicks     = 2
	RoundWaitExtend       = 5
	MaxRoundPostponements = 3
)

// Round is one open contention round for a shared resource.
type Round struct {
	ID            string
	Resource      string
	OpenTick      int64
	CloseTick     int64
	State         string // "collecting" | "waiting"
	Reason        string // why the round is waiting (missing client input)
	Postponements int
	Claims        []Input // collected claims, adjudicated at CloseTick
	Deferred      []Input // inputs waiting for a claim verdict of this round
}

// RoundState is the snapshot view of an open contention round. It is part
// of the snapshot hash so the collection boundary and the wait reason are
// themselves authoritative and replayable.
type RoundState struct {
	ID            string   `json:"id"`
	Resource      string   `json:"resource"`
	State         string   `json:"state"`
	OpenTick      int64    `json:"openTick"`
	CloseTick     int64    `json:"closeTick"`
	Reason        string   `json:"reason,omitempty"`
	Postponements int      `json:"postponements,omitempty"`
	Claimants     []Holder `json:"claimants"`
}

// ClaimantVerdict records how one collected claim was adjudicated.
type ClaimantVerdict struct {
	CharID  string `json:"charId"`
	Seq     int64  `json:"seq"`
	Verdict string `json:"verdict"` // "granted" or a rejection reason
}

// Claimant names one client input involved in a round.
type Claimant struct {
	CharID string `json:"charId"`
	Seq    int64  `json:"seq"`
}

// RoundEvent is the journaled, persistent identity of a contention round:
// "opened" and "closed" are emitted by the engine (and cross-checked
// during replay); "waiting" is emitted by the intake layer when a missing
// client input postpones the collection boundary.
type RoundEvent struct {
	Type      string            `json:"type"` // always "round"
	RoundID   string            `json:"roundId"`
	Resource  string            `json:"resource"`
	State     string            `json:"state"` // opened | waiting | closed
	Tick      int64             `json:"tick"`
	OpenTick  int64             `json:"openTick"`
	CloseTick int64             `json:"closeTick"`
	Reason    string            `json:"reason,omitempty"`
	Verdicts  []ClaimantVerdict `json:"verdicts,omitempty"`
	// Cancelled lists claims that were still missing when the round
	// closed: they are explicitly cancelled, never silently dropped.
	Cancelled []Claimant `json:"cancelled,omitempty"`
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
	// Round is the persistent identity of the contention round that
	// adjudicated the grant.
	Round string `json:"round,omitempty"`
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
	// Regions are the bookable areas group reservations occupy.
	Regions []RegionSpec `json:"regions,omitempty"`
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
	Tick         int64           `json:"tick"`
	Chars        []CharState     `json:"chars"`
	Resources    []ResourceState `json:"resources,omitempty"`
	Rounds       []RoundState    `json:"rounds"`
	Reservations []ResvView      `json:"reservations,omitempty"`
	Hash         string          `json:"hash"`
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
	// Collected marks a claim gathered into a contention round: the grant
	// is decided at the round's collection boundary, not at arrival, so a
	// collected claim is neither a success nor a rejection yet.
	Collected bool `json:"collected,omitempty"`
	// Deferred marks an input whose dependency is a claim still awaiting
	// adjudication; it executes when the round closes.
	Deferred bool `json:"deferred,omitempty"`
	// Derived marks records produced by round adjudication (claim
	// verdicts and deferred executions). They are outcomes, not new
	// inputs, and are not re-fed into the engine during replay.
	Derived bool `json:"derived,omitempty"`
	// RoundID is the persistent identity of the contention round this
	// record belongs to.
	RoundID string `json:"roundId,omitempty"`
}

// TickRecord is one deterministic append-only event in the room journal.
type TickRecord struct {
	Type   string         `json:"type"` // always "tick"
	Tick   int64          `json:"tick"`
	Inputs []AppliedInput `json:"inputs"`
}

type Room struct {
	ID          string
	Cfg         Config
	Tick        int64
	Chars       []*Char // fixed order, keeps iteration deterministic
	Rules       Rules
	byID        map[string]*Char
	walls       map[[2]int]struct{}
	sealed      map[[2]int]struct{}
	resources   map[string]*ResourceState
	resOrder    []string                  // config declaration order, keeps snapshots deterministic
	outcomes    map[string]map[int64]bool // charId -> seq -> accepted, for dependencies
	rounds      map[string]*Round         // resource id -> open contention round
	roundCount  map[string]int            // resource id -> rounds opened so far
	roundEvents []RoundEvent              // drained by the journal after each tick
	regions     map[string]RegionSpec
	regOrder    []string
	resv        map[string]*Reservation
	resvOrder   []string    // submission order, keeps snapshots deterministic
	resvEvents  []ResvEvent // drained by the journal after each tick
}

func NewRoom(id string, cfg Config, chars []*Char) *Room {
	r := &Room{
		ID: id, Cfg: cfg, Chars: chars,
		byID: map[string]*Char{}, walls: map[[2]int]struct{}{}, sealed: map[[2]int]struct{}{},
		resources: map[string]*ResourceState{}, outcomes: map[string]map[int64]bool{},
		rounds: map[string]*Round{}, roundCount: map[string]int{},
		regions: map[string]RegionSpec{}, resv: map[string]*Reservation{},
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
	for _, spec := range cfg.Regions {
		cap := spec.Capacity
		if cap <= 0 {
			cap = 1
		}
		spec.Capacity = cap
		r.regions[spec.ID] = spec
		r.regOrder = append(r.regOrder, spec.ID)
	}
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
		Regions: []RegionSpec{
			{ID: "hall", Name: "中央大厅", Capacity: 2},
			{ID: "garden", Name: "空中花园", Capacity: 1},
			{ID: "vault", Name: "秘境宝库", Capacity: 1},
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
func (r *Room) ApplyTick(inputs []Input) ([]AppliedInput, Snapshot) {
	return r.ApplyTickCtx(inputs, nil)
}

// ApplyTickCtx advances the clock once. Inputs within the batch are
// executed in character declaration order and then seq order, which is
// the same order used during replay. postpones (resource -> reason) asks
// a round whose collection boundary falls on this tick to keep waiting
// for a missing client input instead of closing; the intake layer
// journals the same decision as a "waiting" round event so replay
// re-derives identical boundaries.
func (r *Room) ApplyTickCtx(inputs []Input, postpones map[string]string) ([]AppliedInput, Snapshot) {
	return r.ApplyTickExt(inputs, postpones, nil, nil)
}

// ApplyTickExt advances the clock once, additionally feeding newly
// journaled reservation requests and cancel/release ops. Ops apply first
// (they were journaled to take effect at this tick), then new requests
// enter, then the adjudicator runs — all before regular inputs, in a
// fixed order so replay re-derives identical decisions.
func (r *Room) ApplyTickExt(inputs []Input, postpones map[string]string, resvReqs []ResvRequest, resvOps []ResvOp) ([]AppliedInput, Snapshot) {
	r.Tick++
	for _, op := range resvOps {
		r.applyResvOp(op)
	}
	for _, req := range resvReqs {
		r.addReservation(req)
	}
	r.adjudicateReservations()
	r.sortInputs(inputs)
	applied := make([]AppliedInput, 0, len(inputs))
	for _, c := range r.Chars {
		// Deterministic energy regen, part of the tick so replay matches.
		c.Energy += energyPerTick
		if c.Energy > maxEnergy {
			c.Energy = maxEnergy
		}
	}
	// Close contention rounds whose collection boundary falls on this
	// tick. Their verdicts are part of this tick's record, before any
	// newly arriving inputs (a claim arriving exactly at the boundary
	// joins a fresh round).
	for _, resID := range r.resOrder {
		rd := r.rounds[resID]
		if rd == nil || rd.CloseTick != r.Tick {
			continue
		}
		if reason, ok := postpones[resID]; ok && rd.Postponements < MaxRoundPostponements {
			rd.State = "waiting"
			rd.Reason = reason
			rd.Postponements++
			rd.CloseTick += RoundWaitExtend
			continue
		}
		r.adjudicate(rd, &applied)
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
		r.execute(c, in, &rec)
		rec.X, rec.Y = c.X, c.Y
		applied = append(applied, rec)
		if !rec.Collected && !rec.Deferred {
			// Collected claims and deferred inputs have no outcome yet;
			// it is recorded when the round adjudicates them.
			r.recordOutcome(in.CharID, in.Seq, rec.Accepted)
		}
	}
	return applied, r.Snapshot()
}

// execute runs one input whose seq has been consumed. Claims are
// collected into the open contention round of their resource (opening it
// if needed); inputs depending on a not-yet-adjudicated claim are
// deferred into that round; everything else is decided immediately.
func (r *Room) execute(c *Char, in Input, rec *AppliedInput) {
	if in.Cmd == CmdUp || in.Cmd == CmdDown || in.Cmd == CmdLeft || in.Cmd == CmdRight {
		c.Facing = in.Cmd
	}
	switch {
	case r.deferIntoRound(in, rec):
		// The claim this input relies on is still being adjudicated:
		// neither execute nor reject it yet.
	case r.dependencyFailed(in):
		// The earlier action this input relied on did not succeed (e.g.
		// another client won the shared resource). Refuse with an
		// independent reason; unrelated inputs are unaffected.
		rec.Reason = "dependency-failed"
	case in.Cmd == CmdClaim:
		r.collectClaim(in, rec)
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
	if !rec.Accepted && !rec.Collected && !rec.Deferred &&
		in.RuleVersion > 0 && in.RuleVersion < r.Rules.Version &&
		(rec.Reason == "sealed" || rec.Reason == "insufficient-energy") {
		// Formed under older rules and forbidden by the current ones:
		// the user must re-confirm explicitly under the new rules.
		rec.NeedsReconfirm = true
	}
}

// collectClaim gathers a claim into the open contention round of its
// resource, opening a new round (with a persistent, journaled identity)
// when none is open.
func (r *Room) collectClaim(in Input, rec *AppliedInput) {
	rd := r.rounds[in.Resource]
	if rd == nil {
		r.roundCount[in.Resource]++
		rd = &Round{
			ID:        fmt.Sprintf("%s-r%d", in.Resource, r.roundCount[in.Resource]),
			Resource:  in.Resource,
			OpenTick:  r.Tick,
			CloseTick: r.Tick + RoundCollectTicks,
			State:     "collecting",
		}
		r.rounds[in.Resource] = rd
		r.roundEvents = append(r.roundEvents, RoundEvent{
			Type: "round", RoundID: rd.ID, Resource: rd.Resource, State: "opened",
			Tick: r.Tick, OpenTick: rd.OpenTick, CloseTick: rd.CloseTick,
		})
	}
	rd.Claims = append(rd.Claims, in)
	rec.Collected = true
	rec.RoundID = rd.ID
}

// deferIntoRound parks an input whose declared dependency is a claim
// still awaiting adjudication. The input executes when that round closes.
func (r *Room) deferIntoRound(in Input, rec *AppliedInput) bool {
	if in.DependsOn <= 0 {
		return false
	}
	if _, known := r.outcomes[in.CharID][in.DependsOn]; known {
		return false
	}
	rd := r.roundOfClaim(in.CharID, in.DependsOn)
	if rd == nil {
		return false
	}
	rd.Deferred = append(rd.Deferred, in)
	rec.Deferred = true
	rec.RoundID = rd.ID
	return true
}

func (r *Room) roundOfClaim(charID string, seq int64) *Round {
	for _, resID := range r.resOrder {
		rd := r.rounds[resID]
		if rd == nil {
			continue
		}
		for _, cl := range rd.Claims {
			if cl.CharID == charID && cl.Seq == seq {
				return rd
			}
		}
	}
	return nil
}

// adjudicate closes a contention round: every collected claim is judged
// in stable order (character declaration, then seq) — independent of how
// the claims were batched on the wire — and deferred inputs execute once
// their dependency verdicts are known.
func (r *Room) adjudicate(rd *Round, applied *[]AppliedInput) {
	ev := RoundEvent{
		Type: "round", RoundID: rd.ID, Resource: rd.Resource, State: "closed",
		Tick: r.Tick, OpenTick: rd.OpenTick, CloseTick: rd.CloseTick, Reason: rd.Reason,
	}
	claims := append([]Input(nil), rd.Claims...)
	r.sortInputs(claims)
	for _, in := range claims {
		c := r.byID[in.CharID]
		rec := AppliedInput{
			Tick: r.Tick, SessionID: in.SessionID, CharID: in.CharID,
			Seq: in.Seq, Cmd: in.Cmd, Resource: in.Resource, DependsOn: in.DependsOn,
			RuleVersion: in.RuleVersion, AppliedRuleVersion: r.Rules.Version,
			Derived: true, RoundID: rd.ID,
		}
		r.applyClaim(c, in, &rec, rd.ID)
		if !rec.Accepted && in.RuleVersion > 0 && in.RuleVersion < r.Rules.Version &&
			(rec.Reason == "sealed" || rec.Reason == "insufficient-energy") {
			rec.NeedsReconfirm = true
		}
		rec.X, rec.Y = c.X, c.Y
		*applied = append(*applied, rec)
		r.recordOutcome(in.CharID, in.Seq, rec.Accepted)
		verdict := rec.Reason
		if rec.Accepted {
			verdict = "granted"
		}
		ev.Verdicts = append(ev.Verdicts, ClaimantVerdict{CharID: in.CharID, Seq: in.Seq, Verdict: verdict})
	}
	deferred := append([]Input(nil), rd.Deferred...)
	r.sortInputs(deferred)
	for _, in := range deferred {
		c := r.byID[in.CharID]
		rec := AppliedInput{
			Tick: r.Tick, SessionID: in.SessionID, CharID: in.CharID,
			Seq: in.Seq, Cmd: in.Cmd, Resource: in.Resource, DependsOn: in.DependsOn,
			RuleVersion: in.RuleVersion, AppliedRuleVersion: r.Rules.Version,
			Derived: true, RoundID: rd.ID,
		}
		r.execute(c, in, &rec)
		rec.X, rec.Y = c.X, c.Y
		*applied = append(*applied, rec)
		r.recordOutcome(in.CharID, in.Seq, rec.Accepted)
	}
	delete(r.rounds, rd.Resource)
	r.roundEvents = append(r.roundEvents, ev)
}

func (r *Room) sortInputs(inputs []Input) {
	sort.SliceStable(inputs, func(i, j int) bool {
		ti, tj := r.charIndex(inputs[i].CharID), r.charIndex(inputs[j].CharID)
		if ti != tj {
			return ti < tj
		}
		return inputs[i].Seq < inputs[j].Seq
	})
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

// applyClaim arbitrates one claim on a shared resource at a round's
// collection boundary. Exactly one input can be granted each unit: the
// winner is decided by the round's stable order (character declaration
// order, then seq), never by arrival time or batching.
func (r *Room) applyClaim(c *Char, in Input, rec *AppliedInput, roundID string) {
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
		rs.Holders = append(rs.Holders, Holder{CharID: c.ID, Seq: in.Seq, Tick: r.Tick, Round: roundID})
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
	s := Snapshot{Tick: r.Tick, Chars: make([]CharState, 0, len(r.Chars)), Rounds: make([]RoundState, 0)}
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
		if rd := r.rounds[id]; rd != nil {
			s.Rounds = append(s.Rounds, roundStateOf(rd))
		}
	}
	for _, id := range r.resvOrder {
		s.Reservations = append(s.Reservations, r.resvViewOf(r.resv[id]))
	}
	return s
}

func roundStateOf(rd *Round) RoundState {
	claimants := make([]Holder, 0, len(rd.Claims))
	for _, cl := range rd.Claims {
		claimants = append(claimants, Holder{CharID: cl.CharID, Seq: cl.Seq})
	}
	return RoundState{
		ID: rd.ID, Resource: rd.Resource, State: rd.State,
		OpenTick: rd.OpenTick, CloseTick: rd.CloseTick,
		Reason: rd.Reason, Postponements: rd.Postponements,
		Claimants: claimants,
	}
}

// HasOpenRound reports whether any contention round is still collecting
// or waiting; the tick loop must keep advancing while one is open.
func (r *Room) HasOpenRound() bool { return len(r.rounds) > 0 }

// OpenRounds exposes the currently open contention rounds (resource
// declaration order) so the intake layer can decide whether a boundary
// must wait for a missing client input.
func (r *Room) OpenRounds() []RoundState {
	out := make([]RoundState, 0, len(r.rounds))
	for _, id := range r.resOrder {
		if rd := r.rounds[id]; rd != nil {
			out = append(out, roundStateOf(rd))
		}
	}
	return out
}

// DrainRoundEvents returns the round events produced since the last call
// (round openings and closings, in order). The intake layer journals them
// so the adjudication identity survives restarts.
func (r *Room) DrainRoundEvents() []RoundEvent {
	ev := r.roundEvents
	r.roundEvents = nil
	return ev
}

// Hash is a canonical content hash of a snapshot, independent of Go map
// iteration order.
func (s Snapshot) HashValue() string {
	clone := Snapshot{Tick: s.Tick, Chars: s.Chars, Resources: s.Resources, Rounds: s.Rounds, Reservations: s.Reservations}
	b, _ := json.Marshal(clone)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// JournalEvent is one ordered journal event: either a tick batch or a rule
// switch. Replay consumes events in journal order so a rule change takes
// effect at exactly the same tick as it did live.
type JournalEvent struct {
	Tick    *TickRecord
	Rules   *RulesRecord
	Round   *RoundEvent
	ResvReq *ResvRequest
	ResvOp  *ResvOpRecord
	Resv    *ResvEvent
}

// ReplayJournal rebuilds every snapshot purely from recorded events. The
// room must be constructed from the same initial configuration. Returned
// snapshots include recomputed hashes; callers compare them against the
// live journal.
func ReplayJournal(id string, cfg Config, initial []*Char, events []JournalEvent) (*Room, []Snapshot, error) {
	r := NewRoom(id, cfg, cloneChars(initial))
	out := make([]Snapshot, 0, len(events))
	postpones := map[int64]map[string]string{}
	resvOps := map[int64][]ResvOp{}
	var pendingResvReqs []ResvRequest
	var derivedRounds []RoundEvent
	var derivedResv []ResvEvent
	for _, ev := range events {
		if ev.ResvReq != nil {
			// Journaled between ticks: enters the engine at the next tick,
			// exactly as it did live.
			pendingResvReqs = append(pendingResvReqs, *ev.ResvReq)
			continue
		}
		if ev.ResvOp != nil {
			resvOps[ev.ResvOp.Tick] = append(resvOps[ev.ResvOp.Tick], ResvOp{Op: ev.ResvOp.Op, ID: ev.ResvOp.ID})
			continue
		}
		if ev.Resv != nil {
			// Derived decisions are re-derived by the engine; the journaled
			// record must match exactly.
			if len(derivedResv) == 0 {
				return nil, nil, fmt.Errorf("resv event %s/%s without derived counterpart", ev.Resv.ID, ev.Resv.State)
			}
			d := derivedResv[0]
			derivedResv = derivedResv[1:]
			if d.ID != ev.Resv.ID || d.State != ev.Resv.State || d.Tick != ev.Resv.Tick {
				return nil, nil, fmt.Errorf("resv event diverged: journal %+v vs derived %+v", *ev.Resv, d)
			}
			continue
		}
		if ev.Round != nil {
			rev := ev.Round
			if rev.State == "waiting" {
				// The intake decided to extend the collection boundary;
				// re-apply the same postponement at the same tick.
				m := postpones[rev.Tick]
				if m == nil {
					m = map[string]string{}
					postpones[rev.Tick] = m
				}
				m[rev.Resource] = rev.Reason
				continue
			}
			// Opened/closed events are re-derived by the engine; the
			// journaled identity must match exactly.
			if len(derivedRounds) == 0 {
				return nil, nil, fmt.Errorf("round event %s/%s without derived counterpart", rev.RoundID, rev.State)
			}
			d := derivedRounds[0]
			derivedRounds = derivedRounds[1:]
			if d.RoundID != rev.RoundID || d.State != rev.State || d.Resource != rev.Resource ||
				d.OpenTick != rev.OpenTick || d.CloseTick != rev.CloseTick {
				return nil, nil, fmt.Errorf("round event diverged: journal %+v vs derived %+v", rev, d)
			}
			continue
		}
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
			if a.Derived {
				// Round verdicts and deferred executions are outcomes of
				// adjudication, re-derived by the engine — not inputs.
				continue
			}
			inputs = append(inputs, Input{
				SessionID: a.SessionID, CharID: a.CharID, Seq: a.Seq, Cmd: a.Cmd,
				Resource: a.Resource, DependsOn: a.DependsOn,
				RuleVersion: a.RuleVersion,
			})
		}
		applied, snap := r.ApplyTickExt(inputs, postpones[rec.Tick], pendingResvReqs, resvOps[rec.Tick])
		pendingResvReqs = nil
		delete(postpones, rec.Tick)
		delete(resvOps, rec.Tick)
		derivedRounds = append(derivedRounds, r.DrainRoundEvents()...)
		derivedResv = append(derivedResv, r.DrainResvEvents()...)
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
