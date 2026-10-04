// Package platform wires the deterministic game engine to HTTP: room
// lifecycle, session ownership, input intake, the fixed-rate authority tick
// and the SSE event stream used by reconnecting clients.
package platform

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"gameplatform/internal/game"
	"gameplatform/internal/store"
)

const tickInterval = 100 * time.Millisecond

// roundWindow is the explicit collection boundary of a contention round:
// once a round opens, the authority waits at most this long for every
// participant's claim before cancelling the round. A round that cannot be
// closed is never silently decided by arrival order. The boundary is
// journaled with the round, so replay and restart re-derive the same
// verdict without re-running the clock.
const roundWindow = 2500 * time.Millisecond

// ClaimRound is one contention round for a shared resource. Claims formed
// by any client (online or offline) are collected into the open round;
// the round closes only at an explicit boundary: every room participant
// submitted a claim (adjudicated), the collection deadline passed, or an
// explicit cancel (both cancelled). The adjudication order is stable
// (character declaration order, then seq) and independent of how inputs
// were batched or when they arrived.
type ClaimRound struct {
	ID         string `json:"id"` // persistable identity, e.g. "pass#1"
	Resource   string `json:"resource"`
	Status     string `json:"status"` // open | adjudicated | cancelled
	OpenTick   int64  `json:"openTick"`
	DeadlineMs int64  `json:"deadlineMs"` // unix milli collection boundary
	CloseTick  int64  `json:"closeTick,omitempty"`
	// CloseReason: complete | deadline | manual.
	CloseReason string       `json:"closeReason,omitempty"`
	Claims      []game.Input `json:"claims"`
	// WaitingFor lists the characters whose claim is still missing; while
	// non-empty the round cannot close by completion.
	WaitingFor []string `json:"waitingFor"`
	// Missing records WaitingFor at close time (deadline/manual cancel).
	Missing         []string `json:"missing,omitempty"`
	Winner          string   `json:"winner,omitempty"`
	CancelRequested bool     `json:"-"`
}

// RoundClaimView is the per-claim detail shared with every client.
type RoundClaimView struct {
	CharID      string `json:"charId"`
	Seq         int64  `json:"seq"`
	RuleVersion int    `json:"ruleVersion"`
}

// RoundView is the client-facing, shared view of one contention round.
type RoundView struct {
	ID          string           `json:"id"`
	Resource    string           `json:"resource"`
	Status      string           `json:"status"`
	OpenTick    int64            `json:"openTick"`
	DeadlineMs  int64            `json:"deadlineMs"`
	CloseTick   int64            `json:"closeTick,omitempty"`
	CloseReason string           `json:"closeReason,omitempty"`
	Claims      []RoundClaimView `json:"claims"`
	WaitingFor  []string         `json:"waitingFor"`
	Missing     []string         `json:"missing,omitempty"`
	Winner      string           `json:"winner,omitempty"`
}

func roundView(r *ClaimRound) RoundView {
	v := RoundView{
		ID: r.ID, Resource: r.Resource, Status: r.Status,
		OpenTick: r.OpenTick, DeadlineMs: r.DeadlineMs,
		CloseTick: r.CloseTick, CloseReason: r.CloseReason,
		Claims:     make([]RoundClaimView, 0, len(r.Claims)),
		WaitingFor: append([]string{}, r.WaitingFor...),
		Missing:    r.Missing, Winner: r.Winner,
	}
	for _, c := range r.Claims {
		v.Claims = append(v.Claims, RoundClaimView{CharID: c.CharID, Seq: c.Seq, RuleVersion: c.RuleVersion})
	}
	return v
}

type roundJournalEntry struct {
	Type  string     `json:"type"` // always "round"
	Round ClaimRound `json:"round"`
}

type CharView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
}

type SessionView struct {
	ID     string `json:"id"`
	CharID string `json:"charId"`
}

type queuedJournalEntry struct {
	Type  string     `json:"type"` // always "queued"
	Input game.Input `json:"input"`
}

type snapshotEvent struct {
	Type     string              `json:"type"` // "snapshot"
	Snapshot game.Snapshot       `json:"snapshot"`
	Applied  []game.AppliedInput `json:"applied"`
	Rejected []game.AppliedInput `json:"rejected,omitempty"`
	Rules    game.Rules          `json:"rules"`
	Rounds   []RoundView         `json:"rounds,omitempty"`
}

type tickJournalEntry struct {
	game.TickRecord
	Hash string `json:"hash"`
}

type pubMsg struct {
	id      int64
	tick    int64
	payload []byte
}

// RoomInstance is one authoritative room with its live engine, journal and
// subscriber fan-out.
type RoomInstance struct {
	ID       string
	Cfg      game.Config
	Chars    []*game.Char
	Views    []CharView
	Sessions []SessionView

	mu           sync.Mutex
	eng          *game.Room
	journal      *store.Journal
	pending      []game.Input
	pendingRules *game.Rules      // switched at the next tick boundary
	admitted     map[string]int64 // charId -> highest seq seen by intake
	progress     map[string]int64 // charId -> highest contiguous admitted seq
	seen         map[string]map[int64]struct{}
	waiting      map[string]map[int64]game.Input
	rounds       map[string]*ClaimRound // resource -> open contention round
	roundSeq     map[string]int         // resource -> round counter
	closing      []*ClaimRound          // closed this tick, journaled after it
	roundHistory []*ClaimRound          // recently closed rounds (capped)
	subs         map[int]chan pubMsg
	nextSub      int
	ring         []pubMsg // recent SSE payloads, absolute event ids
	nextEvtID    int64
	lastActive   time.Time
	stopped      bool
}

// EnqueueResult is returned for every input POST.
type EnqueueResult struct {
	Status     string `json:"status"` // accepted | stale | forbidden
	AppliedSeq int64  `json:"appliedSeq"`
	// RuleVersion is the rules version currently in force; clients learn
	// about rule switches even from a plain input ack.
	RuleVersion int `json:"ruleVersion"`
}

func (ri *RoomInstance) enqueue(in game.Input) EnqueueResult {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	return ri.enqueueLocked(in, true)
}

func (ri *RoomInstance) enqueueLocked(in game.Input, persist bool) EnqueueResult {
	curVersion := ri.eng.Rules.Version
	var owner *game.Char
	for _, ch := range ri.Chars {
		if ch.ID == in.CharID {
			owner = ch
			break
		}
	}
	if owner == nil || owner.OwnerSession != in.SessionID {
		applied := int64(0)
		if owner != nil {
			applied = owner.AppliedSeq
		}
		return EnqueueResult{Status: "forbidden", AppliedSeq: applied, RuleVersion: curVersion}
	}
	if in.Seq <= owner.AppliedSeq {
		return EnqueueResult{Status: "stale", AppliedSeq: owner.AppliedSeq, RuleVersion: curVersion}
	}
	if ri.hasBufferedLocked(in.CharID, in.Seq) {
		return EnqueueResult{Status: "stale", AppliedSeq: owner.AppliedSeq, RuleVersion: curVersion}
	}
	if persist {
		if err := ri.journal.Append(queuedJournalEntry{Type: "queued", Input: in}); err != nil {
			panic(fmt.Errorf("room %s queued input journal append: %w", ri.ID, err))
		}
	}
	ri.admitLocked(in)
	ri.lastActive = time.Now()
	return EnqueueResult{Status: "accepted", RuleVersion: curVersion}
}

func (ri *RoomInstance) hasBufferedLocked(charID string, seq int64) bool {
	if ri.seen[charID] != nil {
		if _, ok := ri.seen[charID][seq]; ok {
			return true
		}
	}
	for _, in := range ri.pending {
		if in.CharID == charID && in.Seq == seq {
			return true
		}
	}
	for _, round := range ri.rounds {
		for _, c := range round.Claims {
			if c.CharID == charID && c.Seq == seq {
				return true
			}
		}
	}
	return ri.waiting[charID] != nil && ri.waiting[charID][seq].Seq == seq
}

func (ri *RoomInstance) admitLocked(in game.Input) {
	if highest := ri.admitted[in.CharID]; in.Seq > highest {
		ri.admitted[in.CharID] = in.Seq
	}
	if in.Seq != ri.nextExpectedLocked(in.CharID) {
		ri.markSeenLocked(in.CharID, in.Seq)
		if ri.waiting == nil {
			ri.waiting = map[string]map[int64]game.Input{}
		}
		if ri.waiting[in.CharID] == nil {
			ri.waiting[in.CharID] = map[int64]game.Input{}
		}
		ri.waiting[in.CharID][in.Seq] = in
		return
	}
	ri.admitContiguousLocked(in)
	for {
		nextSeq := ri.nextExpectedLocked(in.CharID)
		queued, ok := ri.waiting[in.CharID][nextSeq]
		if !ok {
			return
		}
		delete(ri.waiting[in.CharID], nextSeq)
		ri.admitContiguousLocked(queued)
	}
}

// admitContiguousLocked admits an input whose seq is the next expected one.
// Claims are diverted into the open contention round for their resource
// (created on demand) instead of the tick batch: a claim is only executed
// once its round closes at an explicit boundary.
func (ri *RoomInstance) admitContiguousLocked(in game.Input) {
	ri.markSeenLocked(in.CharID, in.Seq)
	if ri.progress[in.CharID] < in.Seq {
		ri.progress[in.CharID] = in.Seq
	}
	if in.Cmd == game.CmdClaim {
		ri.joinRoundLocked(in)
		return
	}
	ri.pending = append(ri.pending, in)
}

// joinRoundLocked collects a claim into the open round for its resource,
// assigning the persistable round identity, and journals the round state.
func (ri *RoomInstance) joinRoundLocked(in game.Input) {
	round := ri.rounds[in.Resource]
	if round == nil {
		ri.roundSeq[in.Resource]++
		round = &ClaimRound{
			ID:         fmt.Sprintf("%s#%d", in.Resource, ri.roundSeq[in.Resource]),
			Resource:   in.Resource,
			Status:     "open",
			OpenTick:   ri.eng.Tick + 1,
			DeadlineMs: time.Now().Add(roundWindow).UnixMilli(),
			Claims:     []game.Input{},
		}
		for _, ch := range ri.Chars {
			round.WaitingFor = append(round.WaitingFor, ch.ID)
		}
		ri.rounds[in.Resource] = round
	}
	in.RoundID = round.ID
	round.Claims = append(round.Claims, in)
	for i, id := range round.WaitingFor {
		if id == in.CharID {
			round.WaitingFor = append(round.WaitingFor[:i], round.WaitingFor[i+1:]...)
			break
		}
	}
	if err := ri.journal.Append(roundJournalEntry{Type: "round", Round: *round}); err != nil {
		panic(fmt.Errorf("room %s round journal append: %w", ri.ID, err))
	}
}

func (ri *RoomInstance) markSeenLocked(charID string, seq int64) {
	if ri.seen == nil {
		ri.seen = map[string]map[int64]struct{}{}
	}
	if ri.seen[charID] == nil {
		ri.seen[charID] = map[int64]struct{}{}
	}
	ri.seen[charID][seq] = struct{}{}
}

func (ri *RoomInstance) nextExpectedLocked(charID string) int64 {
	return ri.progress[charID] + 1
}

// runTick advances the authority exactly once and persists/publishes result.
func (ri *RoomInstance) runTick() {
	ri.mu.Lock()
	ri.lastActive = time.Now()
	if ri.pendingRules != nil {
		// A rule switch is journaled before the first tick that uses it so
		// replay re-derives identical accept/reject decisions.
		rules := *ri.pendingRules
		ri.pendingRules = nil
		ri.eng.SetRules(rules)
		if err := ri.journal.Append(game.RulesRecord{Type: "rules", Tick: ri.eng.Tick + 1, Rules: rules}); err != nil {
			panic(fmt.Errorf("room %s rules journal append: %w", ri.ID, err))
		}
	}
	ri.closeDueRoundsLocked()
	batch := ri.pending
	ri.pending = nil
	for _, in := range batch {
		ri.markSeenLocked(in.CharID, in.Seq)
	}
	applied, snap := ri.eng.ApplyTick(batch)
	if applied == nil {
		applied = []game.AppliedInput{}
	}
	snap.Hash = snap.HashValue()
	rejected := make([]game.AppliedInput, 0)
	for _, a := range applied {
		if !a.Accepted {
			rejected = append(rejected, a)
		}
	}
	jentry := tickJournalEntry{
		TickRecord: game.TickRecord{Tick: snap.Tick, Inputs: applied},
		Hash:       snap.Hash,
	}
	jentry.Type = "tick"
	err := ri.journal.Append(jentry)
	if err == nil {
		err = ri.journalClosedRoundsLocked(applied)
	}
	ev := snapshotEvent{Type: "snapshot", Snapshot: snap, Applied: applied, Rejected: rejected, Rules: ri.eng.Rules, Rounds: ri.roundViewsLocked()}
	payload, _ := json.Marshal(ev)
	ri.publishLocked(pubMsg{id: ri.nextEvtID, tick: snap.Tick, payload: payload})
	ri.nextEvtID++
	ri.mu.Unlock()
	if err != nil {
		// Persistence failure must not be papered over: the room cannot
		// honestly claim an event log afterwards.
		panic(fmt.Errorf("room %s journal append: %w", ri.ID, err))
	}
}

const ringCapacity = 512

// closeDueRoundsLocked closes every open round that reached an explicit
// collection boundary and injects its claims into the next tick batch.
// A complete round (every participant collected) is adjudicated; a round
// that hit its deadline or was explicitly cancelled is cancelled instead
// of being silently decided by arrival order.
func (ri *RoomInstance) closeDueRoundsLocked() {
	resources := make([]string, 0, len(ri.rounds))
	for res := range ri.rounds {
		resources = append(resources, res)
	}
	sort.Strings(resources)
	for _, res := range resources {
		round := ri.rounds[res]
		complete := len(round.WaitingFor) == 0
		due := time.Now().UnixMilli() >= round.DeadlineMs
		if !complete && !due && !round.CancelRequested {
			continue
		}
		delete(ri.rounds, res)
		round.CloseTick = ri.eng.Tick + 1
		switch {
		case complete:
			round.Status = "adjudicated"
			round.CloseReason = "complete"
		case round.CancelRequested:
			round.Status = "cancelled"
			round.CloseReason = "manual"
		default:
			round.Status = "cancelled"
			round.CloseReason = "deadline"
		}
		if round.Status == "cancelled" {
			round.Missing = append([]string{}, round.WaitingFor...)
		}
		for _, claim := range round.Claims {
			if round.Status == "cancelled" {
				claim.Cancelled = true
			}
			ri.pending = append(ri.pending, claim)
		}
		ri.closing = append(ri.closing, round)
	}
}

// journalClosedRoundsLocked records the outcome of rounds closed by this
// tick: the winner is read back from the authoritative applied records so
// the journaled verdict can never diverge from the executed one.
func (ri *RoomInstance) journalClosedRoundsLocked(applied []game.AppliedInput) error {
	for _, round := range ri.closing {
		for _, a := range applied {
			if a.RoundID == round.ID && a.Accepted {
				round.Winner = a.CharID
			}
		}
		if err := ri.journal.Append(roundJournalEntry{Type: "round", Round: *round}); err != nil {
			return err
		}
		ri.roundHistory = append(ri.roundHistory, round)
		if len(ri.roundHistory) > 16 {
			ri.roundHistory = ri.roundHistory[len(ri.roundHistory)-16:]
		}
	}
	ri.closing = nil
	return nil
}

// roundViewsLocked builds the shared adjudication view: open rounds first
// (with who is still missing), then recently closed rounds.
func (ri *RoomInstance) roundViewsLocked() []RoundView {
	views := make([]RoundView, 0, len(ri.rounds)+len(ri.roundHistory))
	resources := make([]string, 0, len(ri.rounds))
	for res := range ri.rounds {
		resources = append(resources, res)
	}
	sort.Strings(resources)
	for _, res := range resources {
		views = append(views, roundView(ri.rounds[res]))
	}
	for _, round := range ri.roundHistory {
		views = append(views, roundView(round))
	}
	return views
}

// Rounds returns the current shared adjudication view.
func (ri *RoomInstance) Rounds() []RoundView {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	return ri.roundViewsLocked()
}

// roundClosableLocked reports whether any open round reached an explicit
// collection boundary and therefore needs a tick to close.
func (ri *RoomInstance) roundClosableLocked() bool {
	now := time.Now().UnixMilli()
	for _, round := range ri.rounds {
		if len(round.WaitingFor) == 0 || round.CancelRequested || now >= round.DeadlineMs {
			return true
		}
	}
	return false
}

// CancelRound explicitly cancels an open contention round. The collected
// claims are refused with reason "round-cancelled" at the next tick; the
// resource is not granted to anyone.
func (ri *RoomInstance) CancelRound(roundID string) bool {
	ri.mu.Lock()
	found := false
	for _, round := range ri.rounds {
		if round.ID == roundID {
			round.CancelRequested = true
			found = true
		}
	}
	ri.mu.Unlock()
	if found {
		ri.runTick()
	}
	return found
}

func (ri *RoomInstance) publishLocked(m pubMsg) {
	ri.ring = append(ri.ring, m)
	if len(ri.ring) > ringCapacity {
		rings := ri.ring[len(ri.ring)-ringCapacity:]
		rings0 := make([]pubMsg, len(rings))
		copy(rings0, rings)
		ri.ring = rings0
	}
	// Slow subscribers are disconnected instead of blocking the tick loop.
	for _, ch := range ri.subs {
		select {
		case ch <- m:
		default:
		}
	}
}

func (ri *RoomInstance) subscribe(afterID int64, afterTick int64) (int, chan pubMsg, []pubMsg) {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	id := ri.nextSub
	ri.nextSub++
	ch := make(chan pubMsg, 128)
	ri.subs[id] = ch
	bl := make([]pubMsg, 0, len(ri.ring))
	for _, m := range ri.ring {
		if (afterTick >= 0 && m.tick > afterTick) || (afterTick < 0 && m.id > afterID) {
			bl = append(bl, m)
		}
	}
	return id, ch, bl
}

func (ri *RoomInstance) unsubscribe(id int) {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	if ch, ok := ri.subs[id]; ok {
		delete(ri.subs, id)
		close(ch)
	}
}

func (ri *RoomInstance) snapshot() game.Snapshot {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	s := ri.eng.Snapshot()
	s.Hash = s.HashValue()
	return s
}

// rules returns the rules currently in force.
func (ri *RoomInstance) rules() game.Rules {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	return ri.eng.Rules
}

// SwitchRules schedules a new rules version (sealed region, action costs
// and/or closing resource claims). It takes effect at the very next tick,
// is journaled, and forces one tick immediately so the switch is durable
// and visible even when no inputs are in flight. Returns the new rules.
func (ri *RoomInstance) SwitchRules(sealed [][2]int, costs map[game.Command]int, claimsClosed *bool) game.Rules {
	ri.mu.Lock()
	next := ri.eng.Rules
	next.Version++
	next.Sealed = sealed
	if claimsClosed != nil {
		next.ClaimsClosed = *claimsClosed
	}
	if costs != nil {
		merged := map[game.Command]int{}
		for k, v := range ri.eng.Rules.Costs {
			merged[k] = v
		}
		for k, v := range costs {
			merged[k] = v
		}
		next.Costs = merged
	}
	ri.pendingRules = &next
	ri.mu.Unlock()
	// Force a tick so the switch lands in the journal right away.
	ri.runTick()
	return next
}
