// Package store persists room journals as append-only JSONL files.
package store

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Journal struct {
	mu   sync.Mutex
	path string
	f    *os.File
	w    *bufio.Writer
}

// InitEntry is the first line of every journal file.
type InitEntry struct {
	Type    string                 `json:"type"` // "init"
	RoomID  string                 `json:"roomId"`
	Version int                    `json:"version"`
	Config  json.RawMessage        `json:"config"`
	Chars   json.RawMessage        `json:"chars"`
	Meta    map[string]interface{} `json:"meta,omitempty"`
}

func NewID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

// OpenStore creates dir if needed and opens (or creates) a room journal.
func OpenStore(dir, roomID string, init InitEntry) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, roomID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	info, _ := f.Stat()
	j := &Journal{path: path, f: f, w: bufio.NewWriter(f)}
	if info.Size() == 0 {
		init.Type = "init"
		if err := j.appendLocked(init); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return j, nil
}

func (j *Journal) Append(v interface{}) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.appendLocked(v)
}

func (j *Journal) appendLocked(v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := j.w.Write(b); err != nil {
		return err
	}
	if err := j.w.WriteByte('\n'); err != nil {
		return err
	}
	return j.w.Flush()
}

func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.w.Flush(); err != nil {
		return err
	}
	return j.f.Close()
}

// ReadEntries streams every JSON line of a room journal.
func ReadEntries(dir, roomID string, fn func(line []byte) error) error {
	f, err := os.Open(filepath.Join(dir, roomID+".jsonl"))
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner := sc; scanner.Scan(); {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := fn(append([]byte(nil), line...)); err != nil {
			return err
		}
	}
	return sc.Err()
}

// DecodeInit extracts the init entry from a journal.
func DecodeInit(line []byte) (InitEntry, error) {
	var e InitEntry
	if err := json.Unmarshal(line, &e); err != nil {
		return e, err
	}
	if e.Type != "init" {
		return e, fmt.Errorf("journal does not start with init entry")
	}
	return e, nil
}
