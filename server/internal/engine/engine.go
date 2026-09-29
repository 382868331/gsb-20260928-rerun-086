// Package engine runs live authoritative rooms: fixed-tick simulation,
// idempotent input intake, session/character binding and state broadcast.
package engine

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"gameserver/internal/sim"
	"gameserver/internal/store"
)

const TickInterval = 100 * time.Millisecond

type pendingEffect struct {
	kind    string // input | impulse
	input   store.InputData
	impulse store.ImpulseData
}

type Room struct {
	ID      string
	CharIDs []string
	st      *store.Store
	path    string

	mu      sync.Mutex
	state   *sim.State
	tick    int64
	tokenOf map[string]string         // charId -> token
	charOf  map[string]string         // token  -> charId
	pending map[int64][]pendingEffect // apply-tick -> effects
	subs    map[int64]chan Broadcast
	nextSub int64
	stopCh  chan struct{}
	stopped bool
}

type Broadcast struct {
	Event store.Event
	Frame *Frame // nil for non-physics events
}

type Frame struct {
	Tick  int64                     `json:"tick"`
	Hash  string                    `json:"hash"`
	Chars map[string]*sim.CharState `json:"chars"`
}

type Engine struct {
	mu    sync.Mutex
	rooms map[string]*Room
	dir   string
}

func New(dataDir string) *Engine {
	return &Engine{rooms: map[string]*Room{}, dir: dataDir}
}

func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CreateRoom creates a room with two characters and persists the room event.
func (e *Engine) CreateRoom() (*Room, string, string, error) {
	id := newToken()[:8]
	charA, charB := "alpha", "beta"
	path := e.dir + "/room-" + id + ".jsonl"
	st, err := store.Open(path)
	if err != nil {
		return nil, "", "", err
	}
	r := &Room{
		ID:      id,
		CharIDs: []string{charA, charB},
		st:      st,
		path:    path,
		state:   sim.NewState([]string{charA, charB}),
		tokenOf: map[string]string{},
		charOf:  map[string]string{},
		pending: map[int64][]pendingEffect{},
		subs:    map[int64]chan Broadcast{},
		stopCh:  make(chan struct{}),
	}
	if _, err := st.Append("room", 0, store.RoomData{RoomID: id, CharIDs: r.CharIDs}); err != nil {
		return nil, "", "", err
	}
	tokenA, tokenB := newToken(), newToken()
	r.tokenOf[charA] = tokenA
	r.charOf[tokenA] = charA
	r.tokenOf[charB] = tokenB
	r.charOf[tokenB] = charB
	_, _ = st.Append("session", 0, store.SessionData{Token: tokenA, CharID: charA})
	_, _ = st.Append("session", 0, store.SessionData{Token: tokenB, CharID: charB})
	e.mu.Lock()
	e.rooms[id] = r
	e.mu.Unlock()
	go r.loop()
	return r, tokenA, tokenB, nil
}

func (e *Engine) Get(id string) *Room {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rooms[id]
}

func (r *Room) loop() {
	t := time.NewTicker(TickInterval)
	defer t.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-t.C:
			r.step()
		}
	}
}

func (r *Room) step() {
	r.mu.Lock()
	next := r.tick + 1
	effects := r.pending[next]
	delete(r.pending, next)
	var appliedEvents []store.Event
	for _, ef := range effects {
		switch ef.kind {
		case "input":
			d := ef.input
			c := r.state.Chars[d.CharID]
			if c != nil && d.Seq > c.LastSeq {
				ev, err := r.st.Append("input", next, d)
				if err == nil {
					r.state.ApplyInput(d.CharID, d.Seq, d.DirX, d.DirY)
					appliedEvents = append(appliedEvents, ev)
				}
			}
		case "impulse":
			d := ef.impulse
			ev, err := r.st.Append("impulse", next, d)
			if err == nil {
				r.state.ApplyImpulse(d.CharID, d.DirX, d.DirY)
				appliedEvents = append(appliedEvents, ev)
			}
		}
	}
	r.state.Advance()
	r.tick = next
	hash := r.state.Hash()
	_, _ = r.st.Append("checkpoint", next, store.CheckpointData{Hash: hash})
	frame := &Frame{Tick: next, Hash: hash, Chars: map[string]*sim.CharState{}}
	for k, v := range r.state.Chars {
		c := *v
		frame.Chars[k] = &c
	}
	subs := make([]chan Broadcast, 0, len(r.subs))
	for _, ch := range r.subs {
		subs = append(subs, ch)
	}
	r.mu.Unlock()

	for _, ch := range subs {
		for _, ev := range appliedEvents {
			send(ch, Broadcast{Event: ev})
		}
		send(ch, Broadcast{Frame: frame})
	}
}

func send(ch chan Broadcast, b Broadcast) {
	select {
	case ch <- b:
	default: // slow/disconnected subscriber: drop, it can resume by lastEventId
	}
}

// Authenticate returns the character id bound to the token, or "" if invalid.
func (r *Room) Authenticate(token string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.charOf[token]
}

// Bind assigns an unbound slot to a token. Reconnecting with the same token
// returns the same character. A foreign token can never take a slot over.
func (r *Room) Bind(token string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch, ok := r.charOf[token]; ok {
		return ch, true // resume own character
	}
	for _, ch := range r.CharIDs {
		if _, taken := r.tokenOf[ch]; !taken {
			r.tokenOf[ch] = token
			r.charOf[token] = ch
			_, _ = r.st.Append("session", r.tick, store.SessionData{Token: token, CharID: ch})
			return ch, true
		}
	}
	return "", false // room full: cannot hijack either character
}

// FrameResult is returned to input callers.
type FrameResult struct {
	Tick        int64                     `json:"tick"`
	Hash        string                    `json:"hash"`
	Chars       map[string]*sim.CharState `json:"chars"`
	CharID      string                    `json:"charId"`
	Deduped     bool                      `json:"deduped"`
	ScheduledAt int64                     `json:"scheduledAt"`
}

// SubmitInput queues an input for the next tick. Duplicate or stale sequence
// numbers are accepted on the wire (retransmissions are normal) but never
// applied twice.
func (r *Room) SubmitInput(token string, d store.InputData) FrameResult {
	charID := r.Authenticate(token)
	r.mu.Lock()
	if c := r.state.Chars[charID]; charID != "" && c != nil {
		dedup := d.Seq <= c.LastSeq
		if !dedup {
			for _, ef := range r.pending[r.tick+1] {
				if ef.kind == "input" && ef.input.CharID == charID && ef.input.Seq == d.Seq {
					dedup = true
				}
			}
		}
		if !dedup {
			d.CharID = charID // a client can only drive its own character
			r.pending[r.tick+1] = append(r.pending[r.tick+1], pendingEffect{kind: "input", input: d})
		}
		res := FrameResult{
			Tick: r.tick, Hash: r.state.Hash(),
			Chars: map[string]*sim.CharState{}, CharID: charID,
			Deduped: dedup, ScheduledAt: r.tick + 1,
		}
		for k, v := range r.state.Chars {
			c := *v
			res.Chars[k] = &c
		}
		r.mu.Unlock()
		return res
	}
	r.mu.Unlock()
	return FrameResult{CharID: charID}
}

// ScheduleImpulse queues an authoritative server kick for the next tick.
// It originates only on the server, so clients cannot predict it.
func (r *Room) ScheduleImpulse(d store.ImpulseData) {
	r.mu.Lock()
	r.pending[r.tick+1] = append(r.pending[r.tick+1], pendingEffect{kind: "impulse", impulse: d})
	r.mu.Unlock()
}

// Subscribe registers an SSE consumer. Events with id > lastEventID already in
// the log are replayed first, which covers a brief disconnect without loss.
func (r *Room) Subscribe(lastEventID int64) (int64, <-chan Broadcast, func()) {
	r.mu.Lock()
	id := r.nextSub
	r.nextSub++
	ch := make(chan Broadcast, 64)
	r.subs[id] = ch
	logs := r.st.Events()
	tick, hash := r.tick, r.state.Hash()
	chars := map[string]*sim.CharState{}
	for k, v := range r.state.Chars {
		c := *v
		chars[k] = &c
	}
	r.mu.Unlock()

	go func() {
		for _, ev := range logs {
			if ev.ID > lastEventID && (ev.Kind == "input" || ev.Kind == "impulse") {
				send(ch, Broadcast{Event: ev})
			}
		}
		ch <- Broadcast{Frame: &Frame{Tick: tick, Hash: hash, Chars: chars}}
	}()

	return id, ch, func() {
		r.mu.Lock()
		if c, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(c)
		}
		r.mu.Unlock()
	}
}

func (r *Room) Snapshot() Frame {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := Frame{Tick: r.tick, Hash: r.state.Hash(), Chars: map[string]*sim.CharState{}}
	for k, v := range r.state.Chars {
		c := *v
		f.Chars[k] = &c
	}
	return f
}

func (r *Room) Events() []store.Event { return r.st.Events() }

func (r *Room) Close() {
	r.mu.Lock()
	if !r.stopped {
		r.stopped = true
		close(r.stopCh)
	}
	r.mu.Unlock()
	_ = r.st.Close()
}
