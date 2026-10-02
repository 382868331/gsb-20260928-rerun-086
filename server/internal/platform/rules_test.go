package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

// Mid-session rule switch: inputs formed under the old version keep their
// declared basis, are judged explicitly (reject + reconfirm), later inputs
// are still processed in order, duplicates stay idempotent, and the
// authoritative replay matches the live record — across a restart.
func TestRuleSwitchWithOfflineInputsAndReplay(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	s := info.Sessions[0]
	post := func(seq int64, cmd string, rv int) (int, EnqueueResult) {
		return postInput(t, c, info.ID, map[string]interface{}{
			"sessionId": s.ID, "charId": s.CharID, "seq": seq, "cmd": cmd, "ruleVersion": rv,
		})
	}
	// v1: two moves right from (2,2) -> (4,2).
	if code, _ := post(1, "right", 1); code != 202 {
		t.Fatalf("seq1 code=%d", code)
	}
	if code, _ := post(2, "right", 1); code != 202 {
		t.Fatalf("seq2 code=%d", code)
	}
	time.Sleep(300 * time.Millisecond)

	// Switch to v2: seal (3,2) — the cell char A is heading back through.
	rw := c.do("POST", "/api/rooms/"+info.ID+"/rules", bytes.NewReader([]byte(
		`{"sealed":[[3,2]],"costs":{"up":1,"down":1,"left":1,"right":1,"wait":0}}`)))
	if rw.Code != 200 {
		t.Fatalf("rules switch code=%d body=%s", rw.Code, rw.Body.String())
	}
	var rules struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &rules); err != nil || rules.Version != 2 {
		t.Fatalf("rules version = %+v err=%v", rules, err)
	}

	// Offline-formed inputs arrive late, still declaring basis v1.
	if code, _ := post(3, "left", 1); code != 202 {
		t.Fatalf("seq3 code=%d", code)
	}
	if code, _ := post(4, "left", 1); code != 202 {
		t.Fatalf("seq4 code=%d", code)
	}
	// Duplicate delivery of seq 4 stays idempotent.
	if code, res := post(4, "left", 1); code != 200 || res.Status != "stale" {
		t.Fatalf("duplicate seq4 code=%d res=%+v", code, res)
	}
	time.Sleep(350 * time.Millisecond)

	ri, err := srv.mgr.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	snap := ri.snapshot()
	if snap.Chars[0].AppliedSeq != 4 {
		t.Fatalf("appliedSeq = %d, want 4 (rejection still consumes seq)", snap.Chars[0].AppliedSeq)
	}
	// seq 3 left into sealed (3,2) rejected; seq 4 left into (3,2) also
	// rejected — each with its own reason; char stays at (4,2).
	if snap.Chars[0].X != 4 || snap.Chars[0].Y != 2 {
		t.Fatalf("pos = (%d,%d), want (4,2)", snap.Chars[0].X, snap.Chars[0].Y)
	}

	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Match {
		t.Fatalf("replay after rule switch mismatch: %+v", rep)
	}

	// Restart: restore from journal, rule version survives, replay matches.
	mgr2 := NewManager(srv.mgr.dir)
	if err := mgr2.LoadRoom(info.ID); err != nil {
		t.Fatalf("load room: %v", err)
	}
	ri2, err := mgr2.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := ri2.rules().Version; got != 2 {
		t.Fatalf("restored rules version = %d, want 2", got)
	}
	rep2, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil || !rep2.Match {
		t.Fatalf("replay after restart: %+v err=%v", rep2, err)
	}
}

// The snapshot event stream carries the active rules and per-input
// rejection reasons so clients can show exactly which version judged what.
func TestSnapshotEventCarriesRulesAndReasons(t *testing.T) {
	c, _ := newTestServer(t)
	info := createRoom(t, c)
	s := info.Sessions[0]
	postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": s.ID, "charId": s.CharID, "seq": 1, "cmd": "right", "ruleVersion": 1,
	})
	c.do("POST", "/api/rooms/"+info.ID+"/rules", bytes.NewReader([]byte(`{"sealed":[[3,2]]}`)))
	postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": s.ID, "charId": s.CharID, "seq": 2, "cmd": "left", "ruleVersion": 1,
	})
	time.Sleep(350 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/api/rooms/"+info.ID+"/events?afterTick=0", nil).WithContext(ctx)
	rec := newSSERecorder()
	done := make(chan struct{})
	go func() {
		c.h.ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-rec.flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for SSE backlog")
	}
	cancel()
	<-done
	body := rec.String()
	for _, want := range []string{`"rules"`, `"version":2`, `"reason":"sealed"`, `"needsReconfirm":true`, `"ruleVersion":1`, `"appliedRuleVersion":2`} {
		if !bytes.Contains([]byte(body), []byte(want)) {
			t.Fatalf("snapshot stream missing %s", want)
		}
	}
}
