package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gameserver/internal/engine"
)

// newTestMux wires the real HTTP mux and drives it in-process (no sockets;
// the sandbox forbids listening), exercising routing, auth and handlers.
func newTestMux(t *testing.T) (http.Handler, string, string) {
	t.Helper()
	s := &Server{eng: engine.New(t.TempDir())}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/rooms", s.createRoom)
	mux.HandleFunc("POST /api/rooms/{id}/join", s.join)
	mux.HandleFunc("POST /api/rooms/{id}/inputs", s.input)
	mux.HandleFunc("POST /api/rooms/{id}/impulse", s.impulse)
	mux.HandleFunc("GET /api/rooms/{id}/events", s.events)

	req := httptest.NewRequest("POST", "/api/rooms", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var cr struct {
		RoomID string            `json:"roomId"`
		Tokens map[string]string `json:"tokens"`
	}
	if err := json.NewDecoder(rec.Result().Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	return mux, cr.RoomID, cr.Tokens["alpha"]
}

func call(mux http.Handler, method, path, token, body string) (int, map[string]any) {
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Session-Token", token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestHTTPFlow(t *testing.T) {
	mux, roomID, tokenA := newTestMux(t)
	base := "/api/rooms/" + roomID

	if code, _ := call(mux, "POST", base+"/inputs", "forged", `{"seq":1,"dirX":1}`); code != 401 {
		t.Fatalf("forged token status = %d, want 401", code)
	}

	joinBody, _ := json.Marshal(map[string]string{"token": tokenA})
	if code, body := call(mux, "POST", base+"/join", tokenA, string(joinBody)); code != 200 || body["charId"] != "alpha" {
		t.Fatalf("join = %d char=%v", code, body["charId"])
	}

	// First delivery then a duplicated packet with the same sequence number.
	call(mux, "POST", base+"/inputs", tokenA, `{"seq":1,"dirX":1,"charId":"beta"}`)
	code, body := call(mux, "POST", base+"/inputs", tokenA, `{"seq":1,"dirX":1}`)
	if code != 200 || body["deduped"] != true {
		t.Fatalf("duplicate: code=%d deduped=%v", code, body["deduped"])
	}

	time.Sleep(150 * time.Millisecond)
	_, body = call(mux, "POST", base+"/inputs", tokenA, `{"seq":1,"dirX":1}`)
	if body["deduped"] != true {
		t.Fatal("stale seq accepted again")
	}

	call(mux, "POST", base+"/impulse", tokenA, `{"charId":"alpha","dirX":-1}`)
	time.Sleep(250 * time.Millisecond)

	code, body = call(mux, "GET", base+"/events?replay=1", "", "")
	if code != 200 {
		t.Fatalf("replay code = %d", code)
	}
	raw, _ := json.Marshal(body)
	var rep struct {
		MismatchTicks []int64 `json:"mismatchTicks"`
		LiveHash      string  `json:"liveHash"`
		LoggedHash    string  `json:"loggedHash"`
	}
	_ = json.Unmarshal(raw, &rep)
	if len(rep.MismatchTicks) != 0 {
		t.Fatalf("replay mismatches at ticks %v", rep.MismatchTicks)
	}
	if rep.LiveHash == "" || rep.LiveHash != rep.LoggedHash {
		t.Fatalf("replay hash %q != logged checkpoint hash %q", rep.LiveHash, rep.LoggedHash)
	}
}
