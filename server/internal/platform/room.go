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
	Tick      int64  `json:"tick"`
	SessionID string `json:"sessionId"`
	CharID    string `json:"charId"`
	Seq       int64  `json:"seq"`
	Reason    string `json:"reason"`
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

type pubMsg struct {
	id      int64
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
	pending    []game.Input // admitted inputs plus the deferred gap tail
	subs       map[int]chan pubMsg
	nextSub    int
	ring       []pubMsg // recent SSE payloads, absolute event ids
	nextEvtID  int64
	lastActive time.Time
	stopped    bool
}

// EnqueueResult is returned for every input POST.
type EnqueueResult struct {
	Status     string `json:"status"` // accepted | stale | forbidden
	AppliedSeq int64  `json:"appliedSeq"`
}

func (ri *RoomInstance) enqueue(in game.Input) EnqueueResult {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	for _, ch := range ri.Chars {
		if ch.ID == in.CharID {
			if ch.OwnerSession != in.SessionID {
				return EnqueueResult{Status: "forbidden", AppliedSeq: ch.AppliedSeq}
			}
			// Stale means seq <= authority-confirmed. Newer out-of-order inputs
			// are admitted and buffered, so network reordering cannot drop them.
			if in.Seq <= ch.AppliedSeq {
				return EnqueueResult{Status: "stale", AppliedSeq: ch.AppliedSeq}
			}
			for _, p := range ri.pending {
				// A redelivery of an already-buffered (not yet ticked) input is
				// an idempotent ack and must not be queued a second time.
				if p.CharID == in.CharID && p.SessionID == in.SessionID && p.Seq == in.Seq {
					return EnqueueResult{Status: "stale", AppliedSeq: ch.AppliedSeq}
				}
			}
		}
	}
	ri.pending = append(ri.pending, in)
	ri.lastActive = time.Now()
	return EnqueueResult{Status: "accepted"}
}

// runTick advances the authority exactly once and persists/publishes result.
func (ri *RoomInstance) runTick() {
	ri.mu.Lock()
	ri.lastActive = time.Now()
	batch := ri.pending
	ri.pending = nil
	applied, refusedInputs, deferred, snap := ri.eng.ApplyTick(batch)
	if applied == nil {
		applied = []game.AppliedInput{}
	}
	// The gap tail waits for its missing prefix; later inputs append after it
	// and the engine re-sorts by seq, preserving identity and relative order.
	ri.pending = append(ri.pending, deferred...)
	snap.Hash = snap.HashValue()
	rejected := make([]rejectedInput, 0, len(refusedInputs))
	journalRefused := make([]game.RefusedInput, 0, len(refusedInputs))
	for _, f := range refusedInputs {
		journalRefused = append(journalRefused, f)
		rejected = append(rejected, rejectedInput{
			Tick: f.Tick, SessionID: f.SessionID, CharID: f.CharID, Seq: f.Seq, Reason: f.Reason,
		})
	}
	ev := snapshotEvent{Type: "snapshot", Snapshot: snap, Applied: applied, Rejected: rejected}
	payload, _ := json.Marshal(ev)
	jentry := tickJournalEntry{
		TickRecord: game.TickRecord{Tick: snap.Tick, Inputs: applied, Refused: journalRefused},
		Hash:       snap.Hash,
	}
	jentry.Type = "tick"
	err := ri.journal.Append(jentry)
	ri.publishLocked(pubMsg{id: ri.nextEvtID, payload: payload})
	ri.nextEvtID++
	subs := ri.subs
	ri.mu.Unlock()
	if err != nil {
		// Persistence failure must not be papered over: the room cannot
		// honestly claim an event log afterwards.
		panic(fmt.Errorf("room %s journal append: %w", ri.ID, err))
	}
	_ = subs
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

func (ri *RoomInstance) subscribe(afterID int64) (int, chan pubMsg, []pubMsg) {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	id := ri.nextSub
	ri.nextSub++
	ch := make(chan pubMsg, 128)
	ri.subs[id] = ch
	bl := make([]pubMsg, 0, len(ri.ring))
	for _, m := range ri.ring {
		if m.id > afterID {
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
