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
	seen         map[string]map[int64]struct{}
	waiting      map[string]map[int64]game.Input
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
	ri.pending = append(ri.pending, in)
	ri.markSeenLocked(in.CharID, in.Seq)
	for {
		nextSeq := ri.nextExpectedLocked(in.CharID)
		queued, ok := ri.waiting[in.CharID][nextSeq]
		if !ok {
			return
		}
		delete(ri.waiting[in.CharID], nextSeq)
		ri.pending = append(ri.pending, queued)
		ri.markSeenLocked(in.CharID, nextSeq)
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
	applied := int64(0)
	for _, ch := range ri.Chars {
		if ch.ID == charID {
			applied = ch.AppliedSeq
		}
	}
	nextSeq := applied + 1
	for _, in := range ri.pending {
		if in.CharID == charID && in.Seq == nextSeq {
			nextSeq++
		}
	}
	return nextSeq
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
	batch := ri.pending
	ri.pending = nil
	for _, in := range batch {
		ri.markSeenLocked(in.CharID, in.Seq)
	}
	// A contention round whose collection boundary falls on the next tick
	// must wait when a client's claim is known but stuck behind a missing
	// earlier input: the round cannot close honestly without it. The wait
	// is journaled before the tick so replay re-derives the same boundary.
	postpones, waitEvents := ri.computePostponementsLocked()
	for _, we := range waitEvents {
		if err := ri.journal.Append(we); err != nil {
			panic(fmt.Errorf("room %s round-wait journal append: %w", ri.ID, err))
		}
	}
	applied, snap := ri.eng.ApplyTickCtx(batch, postpones)
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
	ev := snapshotEvent{Type: "snapshot", Snapshot: snap, Applied: applied, Rejected: rejected, Rules: ri.eng.Rules}
	payload, _ := json.Marshal(ev)
	jentry := tickJournalEntry{
		TickRecord: game.TickRecord{Tick: snap.Tick, Inputs: applied},
		Hash:       snap.Hash,
	}
	jentry.Type = "tick"
	err := ri.journal.Append(jentry)
	for _, rev := range ri.eng.DrainRoundEvents() {
		if rev.State == "closed" {
			// Claims still stuck behind a gap when the round closed are
			// explicitly cancelled — named in the journal, never silently
			// dropped. If they arrive later they join a fresh round and
			// receive their own explicit verdict.
			rev.Cancelled = ri.stuckClaimsLocked(rev.Resource)
		}
		if err2 := ri.journal.Append(rev); err2 != nil && err == nil {
			err = err2
		}
	}
	ri.publishLocked(pubMsg{id: ri.nextEvtID, tick: snap.Tick, payload: payload})
	ri.nextEvtID++
	ri.mu.Unlock()
	if err != nil {
		// Persistence failure must not be papered over: the room cannot
		// honestly claim an event log afterwards.
		panic(fmt.Errorf("room %s journal append: %w", ri.ID, err))
	}
}

// computePostponementsLocked decides which rounds cannot close at the
// next tick because a client's claim is stuck in the gap buffer (an
// earlier input of that client has not arrived). Returns the postponement
// map for the engine and the "waiting" round events to journal.
func (ri *RoomInstance) computePostponementsLocked() (map[string]string, []game.RoundEvent) {
	var postpones map[string]string
	var events []game.RoundEvent
	for _, rs := range ri.eng.OpenRounds() {
		if rs.CloseTick != ri.eng.Tick+1 || rs.Postponements >= game.MaxRoundPostponements {
			continue
		}
		for _, ch := range ri.Chars {
			for _, in := range ri.waiting[ch.ID] {
				if in.Cmd != game.CmdClaim || in.Resource != rs.Resource {
					continue
				}
				reason := fmt.Sprintf("等待角色 %s 补齐 seq %d 之前的缺失输入", ch.ID, ri.nextExpectedLocked(ch.ID))
				if postpones == nil {
					postpones = map[string]string{}
				}
				postpones[rs.Resource] = reason
				events = append(events, game.RoundEvent{
					Type: "round", RoundID: rs.ID, Resource: rs.Resource, State: "waiting",
					Tick: ri.eng.Tick + 1, OpenTick: rs.OpenTick,
					CloseTick: rs.CloseTick + game.RoundWaitExtend, Reason: reason,
				})
			}
		}
	}
	return postpones, events
}

// stuckClaimsLocked lists claims for a resource that are still stuck in
// the gap buffer (character declaration order, then seq).
func (ri *RoomInstance) stuckClaimsLocked(resource string) []game.Claimant {
	var out []game.Claimant
	for _, ch := range ri.Chars {
		var seqs []int64
		for _, in := range ri.waiting[ch.ID] {
			if in.Cmd == game.CmdClaim && in.Resource == resource {
				seqs = append(seqs, in.Seq)
			}
		}
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		for _, seq := range seqs {
			out = append(out, game.Claimant{CharID: ch.ID, Seq: seq})
		}
	}
	return out
}

const ringCapacity = 512

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
