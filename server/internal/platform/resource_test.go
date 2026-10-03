package platform

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"gameplatform/internal/game"
	"gameplatform/internal/store"
)

// countAdjudications scans the room journal for claim/rejection records.
func countAdjudications(t *testing.T, srv *Server, roomID string) (grants, exhausted, depRejected int) {
	t.Helper()
	err := store.ReadEntries(srv.mgr.dir, roomID, func(line []byte) error {
		var rec struct {
			Type   string              `json:"type"`
			Inputs []game.AppliedInput `json:"inputs"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			return err
		}
		for _, in := range rec.Inputs {
			switch {
			case in.Cmd == game.CmdClaim && in.Accepted:
				grants++
			case in.Reason == "resource-exhausted":
				exhausted++
			case in.Reason == "dependency-rejected":
				depRejected++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return grants, exhausted, depRejected
}

// Two clients contend for the single shared pass slot while their inputs
// arrive out of order, duplicated, and across a server restart. The
// predetermined adjudication order decides the holder exactly once; late
// confirmations and re-delivery never grant the slot twice, and the
// journaled replay matches the live run bit-for-bit.
func TestSharedResourceAcrossRestartAndBatches(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	s1, s2 := info.Sessions[0], info.Sessions[1]
	post := func(s SessionView, seq int64, cmd string, dep int64) (int, EnqueueResult) {
		return postInput(t, c, info.ID, map[string]interface{}{
			"sessionId": s.ID, "charId": s.CharID, "seq": seq, "cmd": cmd, "dependsOn": dep,
		})
	}

	// Offline-formed inputs arrive out of order: s1's seq 2 (the claim)
	// lands before its seq 1 and must wait in the gap buffer; s2's claim
	// lands in between. A duplicate of s1's claim stays idempotent.
	if code, _ := post(s1, 2, "claim", 0); code != 202 {
		t.Fatalf("s1 seq2 code=%d", code)
	}
	if code, _ := post(s2, 1, "claim", 0); code != 202 {
		t.Fatalf("s2 seq1 code=%d", code)
	}
	if code, _ := post(s1, 1, "right", 0); code != 202 {
		t.Fatalf("s1 seq1 code=%d", code)
	}
	if code, res := post(s1, 2, "claim", 0); code != 200 || res.Status != "stale" {
		t.Fatalf("duplicate s1 seq2 code=%d res=%+v", code, res)
	}
	time.Sleep(350 * time.Millisecond)

	ri, err := srv.mgr.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	snap := ri.snapshot()
	// Character declaration order wins the single slot: s1's character
	// holds it; s2's claim was rejected even though it arrived before
	// s1's seq 1 — arrival order never decides adjudication.
	if snap.PassesLeft != 0 || snap.Chars[0].Passes != 1 || snap.Chars[1].Passes != 0 {
		t.Fatalf("holder wrong: left=%d a=%d b=%d", snap.PassesLeft, snap.Chars[0].Passes, snap.Chars[1].Passes)
	}

	// s2 keeps going: a dependent action on its rejected claim fails with
	// its own reason; an independent action still executes.
	if code, _ := post(s2, 2, "up", 1); code != 202 {
		t.Fatalf("s2 seq2 code=%d", code)
	}
	if code, _ := post(s2, 3, "left", 0); code != 202 {
		t.Fatalf("s2 seq3 code=%d", code)
	}
	time.Sleep(350 * time.Millisecond)

	// Stop the old room's tick loop first: a restart means the old process
	// is gone before the new one loads the journal. Inputs enqueued after
	// this point are journaled as "queued" but never executed by it.
	ri.mu.Lock()
	ri.stopped = true
	ri.mu.Unlock()

	// s1 queues one more claim (rejected later: pool empty), then the
	// server "restarts" with that input still unconfirmed.
	if code, _ := post(s1, 3, "claim", 0); code != 202 {
		t.Fatalf("s1 seq3 code=%d", code)
	}
	mgr2 := NewManager(srv.mgr.dir)
	if err := mgr2.LoadRoom(info.ID); err != nil {
		t.Fatalf("load room: %v", err)
	}
	srv2 := NewServer(srv.mgr.dir)
	srv2.mgr = mgr2
	c2 := testClient{h: srv2.Routes()}
	post2 := func(s SessionView, seq int64, cmd string, dep int64) (int, EnqueueResult) {
		return postInput(t, c2, info.ID, map[string]interface{}{
			"sessionId": s.ID, "charId": s.CharID, "seq": seq, "cmd": cmd, "dependsOn": dep,
		})
	}
	time.Sleep(350 * time.Millisecond)

	// Late duplicate of the already-confirmed winning claim: stale ack,
	// never granted a second time.
	if code, res := post2(s1, 2, "claim", 0); code != 200 || res.Status != "stale" {
		t.Fatalf("late duplicate of granted claim code=%d res=%+v", code, res)
	}
	// Late duplicate of s2's rejected claim is also a no-op.
	if code, res := post2(s2, 1, "claim", 0); code != 200 || res.Status != "stale" {
		t.Fatalf("late duplicate of rejected claim code=%d res=%+v", code, res)
	}
	time.Sleep(250 * time.Millisecond)

	ri2, err := mgr2.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	snap2 := ri2.snapshot()
	if snap2.PassesLeft != 0 || snap2.Chars[0].Passes != 1 || snap2.Chars[1].Passes != 0 {
		t.Fatalf("resource double-granted after restart: left=%d a=%d b=%d",
			snap2.PassesLeft, snap2.Chars[0].Passes, snap2.Chars[1].Passes)
	}
	// s2's independent move executed (one cell left); its dependent move
	// did not move it up.
	if snap2.Chars[1].X != snap.Chars[1].X-1 || snap2.Chars[1].Y != snap.Chars[1].Y {
		t.Fatalf("s2 independent move wrong: (%d,%d) from (%d,%d)",
			snap2.Chars[1].X, snap2.Chars[1].Y, snap.Chars[1].X, snap.Chars[1].Y)
	}

	// The journal tells the same story as the live run: exactly one grant,
	// explicit conflict/dependency rejections, replay hash identical.
	grants, exhausted, depRejected := countAdjudications(t, srv, info.ID)
	if grants != 1 {
		t.Fatalf("journal grants = %d, want exactly 1", grants)
	}
	if exhausted < 2 { // s2 seq1 and s1 seq3 both hit the empty pool
		t.Fatalf("journal resource-exhausted = %d, want >= 2", exhausted)
	}
	if depRejected != 1 {
		t.Fatalf("journal dependency-rejected = %d, want 1", depRejected)
	}
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Match {
		t.Fatalf("replay mismatch: %+v", rep)
	}
}

// Rule switch closing claims while old-basis claims are still in flight:
// they keep their original basis and get an explicit re-confirm request.
func TestClaimsClosedWithInFlightClaims(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	s1 := info.Sessions[0]
	post := func(seq int64, cmd string, rv int) (int, EnqueueResult) {
		return postInput(t, c, info.ID, map[string]interface{}{
			"sessionId": s1.ID, "charId": s1.CharID, "seq": seq, "cmd": cmd, "ruleVersion": rv,
		})
	}
	rw := c.do("POST", "/api/rooms/"+info.ID+"/rules", bytes.NewReader([]byte(
		`{"sealed":[],"costs":{"claim":0},"claimsClosed":true}`)))
	if rw.Code != 200 {
		t.Fatalf("rules switch code=%d body=%s", rw.Code, rw.Body.String())
	}
	var rules struct {
		Version      int  `json:"version"`
		ClaimsClosed bool `json:"claimsClosed"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &rules); err != nil || rules.Version != 2 || !rules.ClaimsClosed {
		t.Fatalf("rules = %+v err=%v", rules, err)
	}
	// Claim formed offline under v1 arrives after the switch.
	if code, _ := post(1, "claim", 1); code != 202 {
		t.Fatalf("claim code=%d", code)
	}
	time.Sleep(350 * time.Millisecond)
	ri, err := srv.mgr.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	snap := ri.snapshot()
	if snap.PassesLeft != 1 || snap.Chars[0].Passes != 0 {
		t.Fatalf("closed claims must not grant: left=%d held=%d", snap.PassesLeft, snap.Chars[0].Passes)
	}
	if snap.Chars[0].AppliedSeq != 1 {
		t.Fatalf("rejected claim must still consume seq: appliedSeq=%d", snap.Chars[0].AppliedSeq)
	}
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil || !rep.Match {
		t.Fatalf("replay after claimsClosed: %+v err=%v", rep, err)
	}
}
