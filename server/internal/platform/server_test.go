package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gameplatform/internal/store"
)

// sseRecorder is an http.ResponseWriter + Flusher used to drive the SSE
// handler without binding a TCP port (sandbox networking is restricted).
type sseRecorder struct {
	header  http.Header
	mu      sync.Mutex
	buf     bytes.Buffer
	flushed chan struct{}
}

func newSSERecorder() *sseRecorder {
	return &sseRecorder{header: http.Header{}, flushed: make(chan struct{}, 256)}
}
func (r *sseRecorder) Header() http.Header { return r.header }
func (r *sseRecorder) WriteHeader(int)     {}
func (r *sseRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, err := r.buf.Write(b)
	select {
	case r.flushed <- struct{}{}:
	default:
	}
	return n, err
}
func (r *sseRecorder) Flush() {
	select {
	case r.flushed <- struct{}{}:
	default:
	}
}
func (r *sseRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

type testClient struct{ h http.Handler }

func (c *testClient) do(method, path string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rw := httptest.NewRecorder()
	c.h.ServeHTTP(rw, req)
	return rw
}

func newTestServer(t *testing.T) (testClient, *Server) {
	t.Helper()
	srv := NewServer(filepath.Join(t.TempDir(), "journals"))
	return testClient{h: srv.Routes()}, srv
}

func createRoom(t *testing.T, c testClient) CreatedRoom {
	t.Helper()
	rw := c.do("POST", "/api/rooms", nil)
	var info CreatedRoom
	if err := json.Unmarshal(rw.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	return info
}

func postInput(t *testing.T, c testClient, roomID string, body map[string]interface{}) (int, EnqueueResult) {
	t.Helper()
	b, _ := json.Marshal(body)
	rw := c.do("POST", "/api/rooms/"+roomID+"/inputs", bytes.NewReader(b))
	var res EnqueueResult
	_ = json.Unmarshal(rw.Body.Bytes(), &res)
	return rw.Code, res
}

func TestHTTPCreateInputStaleAndReplay(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	s1 := info.Sessions[0]
	s2 := info.Sessions[1]

	code, res := postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": s1.ID, "charId": s1.CharID, "seq": 1, "cmd": "right",
	})
	if code != 202 || res.Status != "accepted" {
		t.Fatalf("first input: code=%d res=%+v", code, res)
	}
	code, res = postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": s1.ID, "charId": s1.CharID, "seq": 1, "cmd": "right",
	})
	if code != 200 || res.Status != "stale" {
		t.Fatalf("duplicate input: code=%d res=%+v", code, res)
	}
	code, _ = postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": s2.ID, "charId": s1.CharID, "seq": 2, "cmd": "up",
	})
	if code != 403 {
		t.Fatalf("hijack attempt code = %d, want 403", code)
	}
	code, _ = postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": "nobody", "charId": s1.CharID, "seq": 2, "cmd": "up",
	})
	if code != 403 {
		t.Fatalf("unknown session code = %d, want 403", code)
	}
	time.Sleep(350 * time.Millisecond)
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Match || rep.Ticks < 1 || rep.FinalHash == "" || rep.FinalHash != rep.LiveFinal {
		t.Fatalf("replay mismatch: %+v", rep)
	}
}

func TestSSEBacklog(t *testing.T) {
	c, _ := newTestServer(t)
	info := createRoom(t, c)
	postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": info.Sessions[0].ID, "charId": info.Sessions[0].CharID,
		"seq": 1, "cmd": "right",
	})
	time.Sleep(300 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/api/rooms/"+info.ID+"/events", nil).WithContext(ctx)
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
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %s", ct)
	}
	if body := rec.String(); !strings.Contains(body, "\"tick\"") {
		t.Fatalf("backlog should contain a snapshot: %q", body)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not stop after context cancel")
	}
}

func TestOutOfOrderInputsExecuteInSessionSequence(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	s := info.Sessions[0]
	body := func(seq int64, cmd string) map[string]interface{} {
		return map[string]interface{}{"sessionId": s.ID, "charId": s.CharID, "seq": seq, "cmd": cmd}
	}

	// seq 3/2 arrive first and must wait for the missing predecessor.
	if code, _ := postInput(t, c, info.ID, body(3, "right")); code != 202 {
		t.Fatalf("seq 3 code = %d", code)
	}
	if code, _ := postInput(t, c, info.ID, body(2, "up")); code != 202 {
		t.Fatalf("seq 2 code = %d", code)
	}
	time.Sleep(150 * time.Millisecond)

	if code, _ := postInput(t, c, info.ID, body(1, "right")); code != 202 {
		t.Fatalf("seq 1 code = %d", code)
	}
	if code, _ := postInput(t, c, info.ID, body(1, "right")); code != 200 {
		t.Fatalf("duplicate seq 1 before tick code = %d, want stale 200", code)
	}
	if code, _ := postInput(t, c, info.ID, body(3, "right")); code != 200 {
		t.Fatalf("duplicate seq 3 code = %d, want stale 200", code)
	}
	time.Sleep(350 * time.Millisecond)
	ri, err := srv.mgr.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	me := ri.eng.Chars[0]
	if me.AppliedSeq != 3 {
		t.Fatalf("applied seq = %d, want 3", me.AppliedSeq)
	}
	if me.X != 4 || me.Y != 1 {
		t.Fatalf("position = (%d,%d), want (4,1): seq order must be right, up, right", me.X, me.Y)
	}
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Match || rep.Ticks != 1 {
		t.Fatalf("reordered batch replay = %+v", rep)
	}
}

func TestRuleSwitchPreservesBasisAndProcessesLaterInput(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	s := info.Sessions[0]

	rw := c.do("POST", "/api/rooms/"+info.ID+"/rules", strings.NewReader(`{"version":2}`))
	if rw.Code != 200 {
		t.Fatalf("switch rules: code=%d body=%s", rw.Code, rw.Body.String())
	}
	post := func(seq int64, cmd string, ruleVersion int) (int, EnqueueResult) {
		return postInput(t, c, info.ID, map[string]interface{}{
			"sessionId": s.ID, "charId": s.CharID, "seq": seq, "cmd": cmd, "ruleVersion": ruleVersion,
		})
	}
	if code, res := post(1, "up", 1); code != 202 || res.Status != "accepted" {
		t.Fatalf("old-rule seq1 enqueue: code=%d res=%+v", code, res)
	}
	if code, _ := post(2, "wait", 1); code != 202 {
		t.Fatalf("old-rule seq2 enqueue code=%d", code)
	}
	time.Sleep(350 * time.Millisecond)
	ri, err := srv.mgr.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	me := ri.eng.Chars[0]
	if me.AppliedSeq != 2 || me.DecidedSeq != 2 || me.X != 2 || me.Y != 2 {
		t.Fatalf("later input should process after withheld seq1: x=%d y=%d applied=%d decided=%d", me.X, me.Y, me.AppliedSeq, me.DecidedSeq)
	}
	if !ri.eng.PendingConfirm(s.CharID, 1) {
		t.Fatal("seq1 should remain pending explicit user confirmation")
	}
	if code, _ := post(1, "up", 1); code != 200 {
		t.Fatalf("duplicate withheld input code=%d, want stale 200", code)
	}

	confirmBody := strings.NewReader(`{"sessionId":"` + s.ID + `","charId":"` + s.CharID + `","seq":1,"cmd":"up","ruleVersion":1}`)
	rw = c.do("POST", "/api/rooms/"+info.ID+"/confirmations", confirmBody)
	if rw.Code != 202 {
		t.Fatalf("late confirmation code=%d body=%s", rw.Code, rw.Body.String())
	}
	time.Sleep(150 * time.Millisecond)
	if me.X != 2 || me.Y != 1 {
		t.Fatalf("late confirmation must have its own independent result: x=%d y=%d", me.X, me.Y)
	}
	if ri.eng.PendingConfirm(s.CharID, 1) {
		t.Fatal("confirmed/rejected seq1 must be removed from pending confirmations")
	}
	journal := readJournal(t, srv.mgr.dir, info.ID)
	if !strings.Contains(journal, "rule-version-changed") || !strings.Contains(journal, "confirmed\":true") {
		t.Fatalf("journal must retain original basis and independent confirmation rejection:\n%s", journal)
	}
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Match || rep.Ticks != 2 {
		t.Fatalf("rule switch replay = %+v", rep)
	}
}

func TestPendingRuleConfirmationSurvivesRestart(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	s := info.Sessions[0]
	c.do("POST", "/api/rooms/"+info.ID+"/rules", strings.NewReader(`{"version":2}`))
	postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": s.ID, "charId": s.CharID, "seq": 1, "cmd": "up", "ruleVersion": 1,
	})
	time.Sleep(350 * time.Millisecond)

	mgr2 := NewManager(srv.mgr.dir)
	if err := mgr2.LoadRoom(info.ID); err != nil {
		t.Fatal(err)
	}
	ri, err := mgr2.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ri.eng.PendingConfirm(s.CharID, 1) {
		t.Fatal("restored room lost pending rule confirmation")
	}
	srv2 := NewServer(srv.mgr.dir)
	srv2.mgr = mgr2
	c2 := testClient{h: srv2.Routes()}
	body := strings.NewReader(`{"sessionId":"` + s.ID + `","charId":"` + s.CharID + `","seq":1,"cmd":"up","ruleVersion":1}`)
	if rw := c2.do("POST", "/api/rooms/"+info.ID+"/confirmations", body); rw.Code != 202 {
		t.Fatalf("late confirmation after restart code=%d body=%s", rw.Code, rw.Body.String())
	}
	time.Sleep(350 * time.Millisecond)
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Match || rep.Ticks != 2 {
		t.Fatalf("restarted confirmation replay = %+v", rep)
	}
}

func readJournal(t *testing.T, dir, roomID string) string {
	t.Helper()
	var out strings.Builder
	if err := storeReadEntries(dir, roomID, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func storeReadEntries(dir, roomID string, out *strings.Builder) error {
	return store.ReadEntries(dir, roomID, func(line []byte) error {
		_, err := out.Write(line)
		return err
	})
}
