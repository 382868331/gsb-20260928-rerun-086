package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"gameplatform/internal/game"
	"gameplatform/internal/store"
)

type Manager struct {
	dir string

	mu    sync.RWMutex
	rooms map[string]*RoomInstance
}

func NewManager(dataDir string) *Manager {
	return &Manager{dir: dataDir, rooms: map[string]*RoomInstance{}}
}

type CreatedRoom struct {
	ID        string              `json:"roomId"`
	Config    game.Config         `json:"config"`
	Chars     []CharView          `json:"chars"`
	Sessions  []SessionView       `json:"sessions"`
	Rules     game.Rules          `json:"rules"`
	Resources []game.ResourceSpec `json:"resources"`
}

var ErrNotFound = errors.New("room not found")

// CreateRoom builds a fresh two-character room and seeds its journal.
func (m *Manager) CreateRoom() (*RoomInstance, CreatedRoom, error) {
	roomID := store.NewID("room")
	s1 := store.NewID("sess")
	s2 := store.NewID("sess")
	c1ID := store.NewID("char")
	c2ID := store.NewID("char")
	cfg := game.DefaultConfig()
	chars := []*game.Char{
		{ID: c1ID, Name: "角色 A", Color: "#38bdf8", OwnerSession: s1, X: 2, Y: 2, Facing: game.CmdRight},
		{ID: c2ID, Name: "角色 B", Color: "#fb7185", OwnerSession: s2, X: cfg.Width - 3, Y: cfg.Height - 3, Facing: game.CmdLeft},
	}
	views := []CharView{
		{ID: c1ID, Name: "角色 A", Color: "#38bdf8", X: 2, Y: 2},
		{ID: c2ID, Name: "角色 B", Color: "#fb7185", X: cfg.Width - 3, Y: cfg.Height - 3},
	}
	sessions := []SessionView{{ID: s1, CharID: c1ID}, {ID: s2, CharID: c2ID}}

	cfgJSON, _ := json.Marshal(cfg)
	charJSON, _ := json.Marshal(chars)
	journal, err := store.OpenStore(m.dir, roomID, store.InitEntry{
		RoomID: roomID, Version: 1, Config: cfgJSON, Chars: charJSON,
	})
	if err != nil {
		return nil, CreatedRoom{}, err
	}

	ri := &RoomInstance{
		ID: roomID, Cfg: cfg, Chars: chars, Views: views, Sessions: sessions,
		eng:        game.NewRoom(roomID, cfg, chars),
		journal:    journal,
		admitted:   map[string]int64{},
		progress:   map[string]int64{},
		waiting:    map[string]map[int64]game.Input{},
		rounds:     map[string]*ClaimRound{},
		roundSeq:   map[string]int{},
		subs:       map[int]chan pubMsg{},
		lastActive: time.Now(),
	}
	m.mu.Lock()
	m.rooms[roomID] = ri
	m.mu.Unlock()
	go m.tickLoop(ri)
	return ri, CreatedRoom{ID: roomID, Config: cfg, Chars: views, Sessions: sessions, Rules: ri.eng.Rules, Resources: cfg.Resources}, nil
}

func (m *Manager) tickLoop(ri *RoomInstance) {
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for range t.C {
		ri.mu.Lock()
		if ri.stopped {
			ri.mu.Unlock()
			return
		}
		// A round that reached its collection boundary needs a tick to
		// close even when no new input arrived; rounds still collecting
		// wait without journaling empty ticks.
		hasPending := len(ri.pending) > 0 || ri.roundClosableLocked()
		if hasPending {
			ri.lastActive = time.Now()
		}
		idle := time.Since(ri.lastActive) > 30*time.Minute
		nSubs := len(ri.subs)
		if hasPending {
			ri.mu.Unlock()
			ri.runTick() // journaled snapshot + fan-out
			continue
		}
		// Empty ticks are not journaled but still publish a heartbeat so
		// reconnecting and connected clients see a live authority clock.
		snap := ri.eng.Snapshot()
		snap.Hash = snap.HashValue()
		// Applied must serialize as [] not null: idle heartbeats reach the
		// client too, and a null field crashes the snapshot folding there.
		ev := snapshotEvent{Type: "snapshot", Snapshot: snap, Applied: []game.AppliedInput{}, Rules: ri.eng.Rules, Rounds: ri.roundViewsLocked()}
		payload, _ := json.Marshal(ev)
		ri.publishLocked(pubMsg{id: ri.nextEvtID, tick: snap.Tick, payload: payload})
		ringsNext := ri.nextEvtID
		ri.nextEvtID = ringsNext + 1
		if idle && nSubs == 0 {
			ri.stopped = true
		}
		stop := ri.stopped
		ri.mu.Unlock()
		if stop {
			_ = ri.journal.Close()
			m.mu.Lock()
			delete(m.rooms, ri.ID)
			m.mu.Unlock()
			return
		}
	}
}

func (m *Manager) Get(id string) (*RoomInstance, error) {
	m.mu.RLock()
	ri, ok := m.rooms[id]
	m.mu.RUnlock()
	if ok {
		return ri, nil
	}
	return nil, ErrNotFound
}

// LoadRoom restores a room purely from its persisted journal (replay on
// open) and resumes accepting connections.
func (m *Manager) LoadRoom(id string) error {
	var (
		initEntry store.InitEntry
		events    []game.JournalEvent
		queued    []game.Input
		rounds    []ClaimRound
	)
	err := store.ReadEntries(m.dir, id, func(line []byte) error {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			return err
		}
		switch head.Type {
		case "init":
			e, err := store.DecodeInit(line)
			if err != nil {
				return err
			}
			initEntry = e
		case "tick":
			var e tickJournalEntry
			if err := json.Unmarshal(line, &e); err != nil {
				return err
			}
			rec := e.TickRecord
			events = append(events, game.JournalEvent{Tick: &rec})
		case "rules":
			var e game.RulesRecord
			if err := json.Unmarshal(line, &e); err != nil {
				return err
			}
			rec := e
			events = append(events, game.JournalEvent{Rules: &rec})
		case "queued":
			var e queuedJournalEntry
			if err := json.Unmarshal(line, &e); err != nil {
				return err
			}
			queued = append(queued, e.Input)
		case "round":
			var e roundJournalEntry
			if err := json.Unmarshal(line, &e); err != nil {
				return err
			}
			rounds = append(rounds, e.Round)
		}
		return nil
	})
	if err != nil {
		return err
	}
	var cfg game.Config
	if err := json.Unmarshal(initEntry.Config, &cfg); err != nil {
		return err
	}
	var chars []*game.Char
	if err := json.Unmarshal(initEntry.Chars, &chars); err != nil {
		return err
	}
	restored, _, err := game.ReplayJournal(id, cfg, chars, events)
	if err != nil {
		return err
	}
	appliedSeen := map[string]map[int64]struct{}{}
	nTicks := 0
	for _, ev := range events {
		if ev.Tick == nil {
			continue
		}
		nTicks++
		for _, in := range ev.Tick.Inputs {
			if appliedSeen[in.CharID] == nil {
				appliedSeen[in.CharID] = map[int64]struct{}{}
			}
			appliedSeen[in.CharID][in.Seq] = struct{}{}
		}
	}
	liveQueued := make([]game.Input, 0, len(queued))
	queuedSeen := map[string]map[int64]struct{}{}
	for _, in := range queued {
		if _, ok := appliedSeen[in.CharID][in.Seq]; ok {
			continue
		}
		if queuedSeen[in.CharID] == nil {
			queuedSeen[in.CharID] = map[int64]struct{}{}
		}
		if _, ok := queuedSeen[in.CharID][in.Seq]; ok {
			continue
		}
		queuedSeen[in.CharID][in.Seq] = struct{}{}
		liveQueued = append(liveQueued, in)
	}
	journal, err := store.OpenStore(m.dir, id, store.InitEntry{}) // file exists: no rewrite
	if err != nil {
		return err
	}
	restoredChars := restored.Chars
	views := make([]CharView, len(restoredChars))
	sessions := make([]SessionView, len(chars))
	for i, c := range restoredChars {
		views[i] = CharView{ID: c.ID, Name: c.Name, Color: c.Color, X: c.X, Y: c.Y}
		sessions[i] = SessionView{ID: c.OwnerSession, CharID: c.ID}
	}
	ri := &RoomInstance{
		ID: id, Cfg: cfg, Chars: restoredChars, Views: views, Sessions: sessions,
		eng: restored, journal: journal,
		admitted: map[string]int64{}, progress: map[string]int64{},
		waiting: map[string]map[int64]game.Input{},
		rounds:  map[string]*ClaimRound{}, roundSeq: map[string]int{},
		subs: map[int]chan pubMsg{}, nextEvtID: int64(nTicks), lastActive: time.Now(),
	}
	for _, c := range restoredChars {
		ri.progress[c.ID] = c.AppliedSeq
	}
	// Restore contention-round identity counters and history from the
	// journaled round events, then reopen rounds that were still open.
	// Open rounds are restored as empty shells: their collected claims are
	// re-admitted from the journaled queued entries below, which rejoins
	// them to the same round identity deterministically.
	for _, round := range rounds {
		var n int
		if _, err := fmt.Sscanf(round.ID, round.Resource+"#%d", &n); err == nil && n > ri.roundSeq[round.Resource] {
			ri.roundSeq[round.Resource] = n
		}
	}
	for i := range rounds {
		round := rounds[i]
		if round.Status != "open" {
			delete(ri.rounds, round.Resource)
			history := round
			ri.roundHistory = append(ri.roundHistory, &history)
			continue
		}
		shell := &ClaimRound{
			ID: round.ID, Resource: round.Resource, Status: "open",
			OpenTick: round.OpenTick, DeadlineMs: round.DeadlineMs,
			Claims: []game.Input{}, WaitingFor: []string{},
		}
		for _, ch := range restoredChars {
			shell.WaitingFor = append(shell.WaitingFor, ch.ID)
		}
		ri.rounds[round.Resource] = shell
	}
	if len(ri.roundHistory) > 16 {
		ri.roundHistory = ri.roundHistory[len(ri.roundHistory)-16:]
	}
	for _, in := range liveQueued {
		ri.enqueueLocked(in, false)
	}
	m.mu.Lock()
	m.rooms[id] = ri
	m.mu.Unlock()
	go m.tickLoop(ri)
	return nil
}
