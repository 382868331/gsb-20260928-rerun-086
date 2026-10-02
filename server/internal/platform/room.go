// Package platform wires the deterministic game engine to HTTP: room
// lifecycle, session ownership, input intake, the fixed-rate authority tick
// and the SSE event stream used by reconnecting clients.
package platform

import (
	"encoding/json"
	"fmt"
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

type rejectedInput struct {
	Input            game.Input `json:"input"`
	Reason           string     `json:"reason"`
	Decision         string     `json:"decision,omitempty"`
	EffectiveVersion int        `json:"effectiveVersion,omitempty"`
	Cost             int        `json:"cost,omitempty"`
	Energy           int        `json:"energy,omitempty"`
}

type queuedJournalEntry struct {
	Type  string     `json:"type"` // always "queued"
	Input game.Input `json:"input"`
}

type snapshotEvent struct {
	Type     string              `json:"type"` // "snapshot"
	Snapshot game.Snapshot       `json:"snapshot"`
	Applied  []game.AppliedInput `json:"applied"`
	Rejected []rejectedInput     `json:"rejected,omitempty"`
}

type tickJournalEntry struct {
	game.TickRecord
	Hash string `json:"hash"`
}

type ruleJournalEntry struct {
	game.RuleRecord
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

	mu         sync.Mutex
	eng        *game.Room
	journal    *store.Journal
	pending    []game.Input
	admitted   map[string]int64 // charId -> highest seq seen by intake
	seen       map[string]map[int64]struct{}
	waiting    map[string]map[int64]game.Input
	subs       map[int]chan pubMsg
	nextSub    int
	ring       []pubMsg // recent SSE payloads, absolute event ids
	nextEvtID  int64
	lastActive time.Time
	stopped    bool
}

// EnqueueResult is returned for every input POST.
type EnqueueResult struct {
	Status      string `json:"status"` // accepted | stale | forbidden
	AppliedSeq  int64  `json:"appliedSeq"`
	DecidedSeq  int64  `json:"decidedSeq,omitempty"`
	RuleVersion int    `json:"ruleVersion,omitempty"`
}

func (ri *RoomInstance) enqueue(in game.Input) EnqueueResult {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	return ri.enqueueLocked(in, true)
}

func (ri *RoomInstance) enqueueLocked(in game.Input, persist bool) EnqueueResult {
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
		return EnqueueResult{Status: "forbidden", AppliedSeq: applied}
	}
	if in.Seq <= owner.DecidedSeq {
		return EnqueueResult{Status: "stale", AppliedSeq: owner.AppliedSeq, DecidedSeq: owner.DecidedSeq, RuleVersion: ri.eng.CurrentRules().Version}
	}
	if ri.hasBufferedLocked(in.CharID, in.Seq) {
		return EnqueueResult{Status: "stale", AppliedSeq: owner.AppliedSeq}
	}
	if persist {
		if err := ri.journal.Append(queuedJournalEntry{Type: "queued", Input: in}); err != nil {
			panic(fmt.Errorf("room %s queued input journal append: %w", ri.ID, err))
		}
	}
	ri.admitLocked(in)
	ri.lastActive = time.Now()
	return EnqueueResult{Status: "accepted", RuleVersion: ri.eng.CurrentRules().Version}
}

func (ri *RoomInstance) switchRules(rules game.Rules) (game.Snapshot, error) {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	rec := ri.eng.ApplyRules(rules)
	if err := ri.journal.Append(ruleJournalEntry{RuleRecord: rec}); err != nil {
		return game.Snapshot{}, err
	}
	snap := ri.eng.Snapshot()
	snap.Hash = snap.HashValue()
	ev := snapshotEvent{Type: "snapshot", Snapshot: snap, Applied: []game.AppliedInput{}}
	payload, _ := json.Marshal(ev)
	ri.publishLocked(pubMsg{id: ri.nextEvtID, tick: snap.Tick, payload: payload})
	ri.nextEvtID++
	return snap, nil
}

func (ri *RoomInstance) confirm(in game.Input) (EnqueueResult, snapshotEvent, bool) {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	in.Confirmed = true
	applied, snap, ok := ri.eng.ConfirmInput(in)
	if !ok {
		var owner *game.Char
		for _, ch := range ri.eng.Chars {
			if ch.ID == in.CharID {
				owner = ch
			}
		}
		appliedSeq, decidedSeq := int64(0), int64(0)
		if owner != nil {
			appliedSeq, decidedSeq = owner.AppliedSeq, owner.DecidedSeq
		}
		return EnqueueResult{Status: "stale", AppliedSeq: appliedSeq, DecidedSeq: decidedSeq}, snapshotEvent{}, false
	}
	snap.Hash = snap.HashValue()
	appliedList := []game.AppliedInput{applied}
	ev := snapshotEvent{Type: "snapshot", Snapshot: snap, Applied: appliedList}
	if !applied.Accepted {
		ev.Rejected = []rejectedInput{{
			Input:  game.Input{SessionID: applied.SessionID, CharID: applied.CharID, Seq: applied.Seq, Cmd: applied.Cmd, RuleVersion: applied.RuleVersion, Confirmed: true},
			Reason: applied.Reason, Decision: applied.Decision, EffectiveVersion: applied.EffectiveVersion, Cost: applied.Cost, Energy: applied.Energy,
		}}
	}
	payload, _ := json.Marshal(ev)
	jentry := tickJournalEntry{
		TickRecord: game.TickRecord{Type: "tick", Tick: snap.Tick, Inputs: appliedList}, Hash: snap.Hash,
	}
	if err := ri.journal.Append(jentry); err != nil {
		panic(fmt.Errorf("room %s confirmation journal append: %w", ri.ID, err))
	}
	ri.publishLocked(pubMsg{id: ri.nextEvtID, tick: snap.Tick, payload: payload})
	ri.nextEvtID++
	var owner *game.Char
	for _, ch := range ri.eng.Chars {
		if ch.ID == in.CharID {
			owner = ch
		}
	}
	return EnqueueResult{Status: "accepted", AppliedSeq: owner.AppliedSeq, DecidedSeq: owner.DecidedSeq, RuleVersion: snap.RuleVersion}, ev, true
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
	for ri.eng.PendingConfirm(charID, nextSeq) {
		nextSeq++
	}
	for _, in := range ri.pending {
		if in.CharID == charID && in.Seq == nextSeq {
			nextSeq++
			for ri.eng.PendingConfirm(charID, nextSeq) {
				nextSeq++
			}
		}
	}
	return nextSeq
}

// runTick advances the authority exactly once and persists/publishes result.
func (ri *RoomInstance) runTick() {
	ri.mu.Lock()
	ri.lastActive = time.Now()
	batch := ri.pending
	ri.pending = nil
	for _, in := range batch {
		ri.markSeenLocked(in.CharID, in.Seq)
	}
	applied, snap := ri.eng.ApplyTick(batch)
	ri.releaseWaitingLocked()
	if applied == nil {
		applied = []game.AppliedInput{}
	}
	snap.Hash = snap.HashValue()
	rejected := make([]rejectedInput, 0)
	for _, a := range applied {
		if !a.Accepted {
			rejected = append(rejected, rejectedInput{
				Input:  game.Input{SessionID: a.SessionID, CharID: a.CharID, Seq: a.Seq, Cmd: a.Cmd, RuleVersion: a.RuleVersion, Confirmed: a.Confirmed},
				Reason: a.Reason, Decision: a.Decision, EffectiveVersion: a.EffectiveVersion, Cost: a.Cost, Energy: a.Energy,
			})
		}
	}
	ev := snapshotEvent{Type: "snapshot", Snapshot: snap, Applied: applied, Rejected: rejected}
	payload, _ := json.Marshal(ev)
	jentry := tickJournalEntry{
		TickRecord: game.TickRecord{Tick: snap.Tick, Inputs: applied},
		Hash:       snap.Hash,
	}
	jentry.Type = "tick"
	err := ri.journal.Append(jentry)
	ri.publishLocked(pubMsg{id: ri.nextEvtID, tick: snap.Tick, payload: payload})
	ri.nextEvtID++
	ri.mu.Unlock()
	if err != nil {
		// Persistence failure must not be papered over: the room cannot
		// honestly claim an event log afterwards.
		panic(fmt.Errorf("room %s journal append: %w", ri.ID, err))
	}
}

func (ri *RoomInstance) releaseWaitingLocked() {
	for {
		moved := false
		for charID, wait := range ri.waiting {
			nextSeq := ri.nextExpectedLocked(charID)
			in, ok := wait[nextSeq]
			if !ok {
				continue
			}
			delete(wait, nextSeq)
			ri.pending = append(ri.pending, in)
			ri.markSeenLocked(charID, nextSeq)
			moved = true
		}
		if !moved {
			return
		}
	}
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
