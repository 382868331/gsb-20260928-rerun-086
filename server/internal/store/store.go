// Package store persists the authoritative event stream (event sourcing).
//
// Events are appended to an in-memory log and mirrored to a JSONL file so the
// exact stream can be replayed. The simulation result is fully determined by
// this stream; nothing is smoothed or derived from wall clocks.
package store

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
)

type Event struct {
	ID   int64           `json:"id"`
	Kind string          `json:"kind"`   // room | session | input | impulse
	At   int64           `json:"atTick"` // server tick when recorded
	Data json.RawMessage `json:"data"`
}

type RoomData struct {
	RoomID  string   `json:"roomId"`
	CharIDs []string `json:"charIds"`
}

type SessionData struct {
	Token  string `json:"token"`
	CharID string `json:"charId"`
}

type InputData struct {
	CharID string `json:"charId"`
	Seq    int64  `json:"seq"`
	DirX   int64  `json:"dirX"`
	DirY   int64  `json:"dirY"`
}

type ImpulseData struct {
	CharID string `json:"charId"`
	DirX   int64  `json:"dirX"`
	DirY   int64  `json:"dirY"`
}

// CheckpointData records the authoritative hash at every tick so a replay can
// advance through ticks that carried no input and verify its own result.
type CheckpointData struct {
	Hash string `json:"hash"`
}

// Store is an append-only event log for one room/run.
type Store struct {
	mu     sync.Mutex
	events []Event
	f      *os.File
	w      *bufio.Writer
}

func Open(path string) (*Store, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s := &Store{f: f, w: bufio.NewWriter(f)}
	if err := s.loadExisting(path); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) loadExisting(path string) error {
	rf, err := os.Open(path)
	if err != nil {
		return err
	}
	defer rf.Close()
	sc := bufio.NewScanner(rf)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		s.events = append(s.events, e)
	}
	return sc.Err()
}

func (s *Store) Append(kind string, atTick int64, payload any) (Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := Event{ID: int64(len(s.events)) + 1, Kind: kind, At: atTick, Data: raw}
	s.events = append(s.events, e)
	b, err := json.Marshal(e)
	if err != nil {
		return Event{}, err
	}
	if _, err := s.w.Write(b); err != nil {
		return Event{}, err
	}
	if err := s.w.WriteByte('\n'); err != nil {
		return Event{}, err
	}
	if err := s.w.Flush(); err != nil {
		return Event{}, err
	}
	return e, s.f.Sync()
}

func (s *Store) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.w.Flush(); err != nil {
		return err
	}
	return s.f.Close()
}
