package platform

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"gameplatform/internal/game"
)

func postResv(t *testing.T, c testClient, roomID string, body map[string]interface{}) (int, ReservationResult) {
	t.Helper()
	raw, _ := json.Marshal(body)
	rw := c.do("POST", "/api/rooms/"+roomID+"/reservations", bytes.NewReader(raw))
	var res ReservationResult
	_ = json.Unmarshal(rw.Body.Bytes(), &res)
	return rw.Code, res
}

func postResvOp(t *testing.T, c testClient, roomID, op, rid string, body map[string]interface{}) int {
	t.Helper()
	raw, _ := json.Marshal(body)
	rw := c.do("POST", "/api/rooms/"+roomID+"/reservations/"+rid+"/"+op, bytes.NewReader(raw))
	return rw.Code
}

func waitResvState(t *testing.T, ri *RoomInstance, id, want string) game.ReservationView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap := ri.snapshot()
		for _, v := range snap.Reservations {
			if v.ID == id && v.State == want {
				return v
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	snap := ri.snapshot()
	t.Fatalf("reservation %s did not reach state %s; snapshot=%+v", id, want, snap.Reservations)
	return game.ReservationView{}
}

// Group reservations over HTTP: idempotent submit, atomic adjudication
// with conflict reporting, cancel-before-start, late-cancel no-op and
// early return — all visible in the authoritative snapshot.
func TestReservationHTTPFlow(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	ri, err := srv.mgr.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess := info.Sessions[0].ID
	c1, c2 := info.Sessions[0].CharID, info.Sessions[1].CharID
	now := ri.snapshot().Tick

	// Unknown session is refused.
	code, _ := postResv(t, c, info.ID, map[string]interface{}{
		"sessionId": "sess_nope", "requestId": "x",
		"chars": []string{c1}, "regions": []string{"garden"}, "start": now + 2, "end": now + 6,
	})
	if code != 403 {
		t.Fatalf("unknown session: code=%d want 403", code)
	}
	// Unknown region is refused.
	code, _ = postResv(t, c, info.ID, map[string]interface{}{
		"sessionId": sess, "requestId": "x",
		"chars": []string{c1}, "regions": []string{"nowhere"}, "start": now + 2, "end": now + 6,
	})
	if code != 400 {
		t.Fatalf("unknown region: code=%d want 400", code)
	}

	// A group request occupying two regions is approved atomically.
	code, res := postResv(t, c, info.ID, map[string]interface{}{
		"sessionId": sess, "requestId": "team-1", "priority": 5,
		"chars": []string{c1, c2}, "regions": []string{"hall", "garden"},
		"start": now + 8, "end": now + 200,
	})
	if code != 202 || res.Status != "accepted" {
		t.Fatalf("submit team-1: code=%d status=%s", code, res.Status)
	}
	// Duplicate submit keeps the identity and never double-books.
	code, res = postResv(t, c, info.ID, map[string]interface{}{
		"sessionId": sess, "requestId": "team-1", "priority": 5,
		"chars": []string{c1, c2}, "regions": []string{"hall", "garden"},
		"start": now + 8, "end": now + 200,
	})
	if code != 200 || res.Status != "duplicate" {
		t.Fatalf("duplicate submit: code=%d status=%s want 200 duplicate", code, res.Status)
	}
	waitResvState(t, ri, "team-1", "approved")

	// A competing request for garden (capacity 1) stays pending as a
	// whole and reports the actual conflicting interval.
	code, _ = postResv(t, c, info.ID, map[string]interface{}{
		"sessionId": sess, "requestId": "team-2", "priority": 1,
		"chars": []string{c2}, "regions": []string{"garden"},
		"start": now + 10, "end": now + 300,
	})
	if code != 202 {
		t.Fatalf("submit team-2: code=%d", code)
	}
	time.Sleep(300 * time.Millisecond)
	v2 := waitResvState(t, ri, "team-2", "pending")
	if len(v2.Conflicts) == 0 {
		t.Fatalf("team-2 pending without reported conflicts: %+v", v2)
	}
	var hasRegion, hasChar bool
	for _, cf := range v2.Conflicts {
		if cf.Kind == "region" && cf.ID == "garden" && cf.By == "team-1" {
			hasRegion = true
		}
		if cf.Kind == "char" && cf.ID == c2 && cf.By == "team-1" {
			hasChar = true
		}
	}
	if !hasRegion || !hasChar {
		t.Fatalf("team-2 conflicts = %+v, want garden region and char conflicts by team-1", v2.Conflicts)
	}

	// Cancelling team-1 before it starts frees capacity for team-2.
	if code := postResvOp(t, c, info.ID, "cancel", "team-1", map[string]interface{}{"sessionId": sess}); code != 202 {
		t.Fatalf("cancel team-1: code=%d", code)
	}
	waitResvState(t, ri, "team-1", "cancelled")
	waitResvState(t, ri, "team-2", "approved")

	// A late cancel against the now-active team-2 is an explicit no-op.
	waitResvState(t, ri, "team-2", "active")
	if code := postResvOp(t, c, info.ID, "cancel", "team-2", map[string]interface{}{"sessionId": sess}); code != 202 {
		t.Fatalf("late cancel team-2: code=%d", code)
	}
	time.Sleep(300 * time.Millisecond)
	if got := waitResvState(t, ri, "team-2", "active"); got.State != "active" {
		t.Fatalf("late cancel changed state to %s", got.State)
	}

	// Early return releases the remaining window; a duplicate return is
	// an idempotent no-op.
	if code := postResvOp(t, c, info.ID, "return", "team-2", map[string]interface{}{"sessionId": sess}); code != 202 {
		t.Fatalf("return team-2: code=%d", code)
	}
	waitResvState(t, ri, "team-2", "released")
	if code := postResvOp(t, c, info.ID, "return", "team-2", map[string]interface{}{"sessionId": sess}); code != 202 {
		t.Fatalf("duplicate return: code=%d", code)
	}
	time.Sleep(250 * time.Millisecond)
	if got := waitResvState(t, ri, "team-2", "released"); got.State != "released" {
		t.Fatalf("duplicate return changed state to %s", got.State)
	}

	// Replay from the journal matches the live run.
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !rep.Match {
		t.Fatalf("replay mismatch: %+v", rep)
	}
}

// After a restart, request identities and closed verdicts are preserved
// and pending requests keep their original identity without re-consuming
// capacity.
func TestReservationRestoreAfterRestart(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	ri, err := srv.mgr.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess := info.Sessions[0].ID
	c1 := info.Sessions[0].CharID
	now := ri.snapshot().Tick
	postResv(t, c, info.ID, map[string]interface{}{
		"sessionId": sess, "requestId": "keep-1", "priority": 3,
		"chars": []string{c1}, "regions": []string{"vault"},
		"start": now + 2, "end": now + 60,
	})
	waitResvState(t, ri, "keep-1", "approved")

	// Simulate a process restart with a fresh manager over the same dir.
	// Stop the old tick loop first: a crashed process cannot keep writing
	// to the journal the restarted one owns.
	ri.mu.Lock()
	ri.stopped = true
	ri.mu.Unlock()
	mgr2 := NewManager(srv.mgr.dir)
	if err := mgr2.LoadRoom(info.ID); err != nil {
		t.Fatalf("load room: %v", err)
	}
	ri2, err := mgr2.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := waitResvState(t, ri2, "keep-1", "approved")
	if v.Start != now+2 || v.End != now+60 {
		t.Fatalf("restored window = [%d,%d), want [%d,%d)", v.Start, v.End, now+2, now+60)
	}

	// Resubmitting the same request id after the restart is a duplicate:
	// identity and verdict are preserved, capacity is not re-consumed.
	s2 := NewServer(srv.mgr.dir)
	s2.mgr = mgr2
	c2 := testClient{h: s2.Routes()}
	code, res := postResv(t, c2, info.ID, map[string]interface{}{
		"sessionId": sess, "requestId": "keep-1", "priority": 3,
		"chars": []string{c1}, "regions": []string{"vault"},
		"start": now + 2, "end": now + 60,
	})
	if code != 200 || res.Status != "duplicate" {
		t.Fatalf("resubmit after restart: code=%d status=%s want 200 duplicate", code, res.Status)
	}
	// A different request for the same window must see vault as occupied.
	code, _ = postResv(t, c2, info.ID, map[string]interface{}{
		"sessionId": sess, "requestId": "keep-2", "priority": 1,
		"chars": []string{info.Sessions[1].CharID}, "regions": []string{"vault"},
		"start": now + 3, "end": now + 50,
	})
	if code != 202 {
		t.Fatalf("submit keep-2: code=%d", code)
	}
	time.Sleep(300 * time.Millisecond)
	v2 := waitResvState(t, ri2, "keep-2", "pending")
	if len(v2.Conflicts) == 0 || v2.Conflicts[0].By != "keep-1" {
		t.Fatalf("keep-2 conflicts = %+v, want blocked by keep-1", v2.Conflicts)
	}
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !rep.Match {
		t.Fatalf("replay mismatch after restart: %+v", rep)
	}
}
