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
