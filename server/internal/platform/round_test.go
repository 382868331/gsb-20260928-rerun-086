package platform

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func openRounds(t *testing.T, ri *RoomInstance) []RoundView {
	t.Helper()
	var open []RoundView
	for _, r := range ri.Rounds() {
		if r.Status == "open" {
			open = append(open, r)
		}
	}
	return open
}

func closedRound(t *testing.T, ri *RoomInstance, id string) RoundView {
	t.Helper()
	for _, r := range ri.Rounds() {
		if r.ID == id && r.Status != "open" {
			return r
		}
	}
	t.Fatalf("round %s not closed: %+v", id, ri.Rounds())
	return RoundView{}
}

func journalEntries(t *testing.T, c testClient, roomID string) []map[string]interface{} {
	t.Helper()
	rw := c.do("GET", "/api/rooms/"+roomID+"/journal", nil)
	var out []map[string]interface{}
	for _, line := range bytes.Split(rw.Body.Bytes(), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var e map[string]interface{}
		if json.Unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// A claim is collected into a contention round and stays undecided while
// the other participant's input is missing; only when the collection
// boundary is reached (both claims collected) is the round adjudicated,
// with a stable, journaled round identity shared by both verdicts.
func TestRoundCollectsBeforeAdjudicating(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	sA, sB := info.Sessions[0], info.Sessions[1]

	postClaim(t, c, info.ID, sA, 1, "pass", 1)
	time.Sleep(300 * time.Millisecond)

	ri, err := srv.mgr.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A's claim must NOT be granted early: the round is still waiting on B.
	pass := passState(t, ri)
	if len(pass.Holders) != 0 || pass.Remaining != 1 {
		t.Fatalf("claim granted before the round closed: %+v", pass)
	}
	open := openRounds(t, ri)
	if len(open) != 1 || open[0].Resource != "pass" {
		t.Fatalf("open rounds = %+v", open)
	}
	if len(open[0].WaitingFor) != 1 || open[0].WaitingFor[0] != sB.CharID {
		t.Fatalf("waitingFor = %+v, want B", open[0].WaitingFor)
	}
	if len(open[0].Claims) != 1 || open[0].Claims[0].CharID != sA.CharID {
		t.Fatalf("collected claims = %+v, want A only", open[0].Claims)
	}
	roundID := open[0].ID

	// B's claim completes the collection boundary; the round adjudicates
	// at the next tick with the stable order (char declaration, then seq).
	postClaim(t, c, info.ID, sB, 1, "pass", 1)
	time.Sleep(350 * time.Millisecond)

	pass = passState(t, ri)
	if len(pass.Holders) != 1 || pass.Holders[0].CharID != sA.CharID {
		t.Fatalf("pass = %+v, want single holder A", pass)
	}
	closed := closedRound(t, ri, roundID)
	if closed.Status != "adjudicated" || closed.CloseReason != "complete" || closed.Winner != sA.CharID {
		t.Fatalf("closed round = %+v", closed)
	}

	// Both verdicts carry the same persistable round identity.
	var aRec, bRec map[string]interface{}
	roundJournaled := false
	for _, e := range journalEntries(t, c, info.ID) {
		if e["type"] == "round" {
			roundJournaled = true
		}
		if e["type"] != "tick" {
			continue
		}
		for _, raw := range e["inputs"].([]interface{}) {
			in := raw.(map[string]interface{})
			if in["cmd"] != "claim" {
				continue
			}
			if in["charId"] == sA.CharID {
				aRec = in
			}
			if in["charId"] == sB.CharID {
				bRec = in
			}
		}
	}
	if !roundJournaled {
		t.Fatal("no round events in journal")
	}
	if aRec["roundId"] != roundID || bRec["roundId"] != roundID {
		t.Fatalf("verdicts lack the shared round id: A=%v B=%v want %s", aRec["roundId"], bRec["roundId"], roundID)
	}
	if aRec["accepted"] != true || bRec["reason"] != "resource-exhausted" {
		t.Fatalf("verdicts: A=%+v B=%+v", aRec, bRec)
	}
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil || !rep.Match {
		t.Fatalf("replay = %+v err=%v", rep, err)
	}
}

// The same two logical claims, sent as one batch or split across several
// ticks, produce the identical final ownership and rejection basis.
func TestRoundAdjudicationIndependentOfBatching(t *testing.T) {
	type outcome struct {
		winnerIdx   int
		loserReason string
	}
	run := func(split bool) outcome {
		c, srv := newTestServer(t)
		info := createRoom(t, c)
		sA, sB := info.Sessions[0], info.Sessions[1]
		if split {
			// B's claim arrives several ticks before A's.
			postClaim(t, c, info.ID, sB, 1, "pass", 1)
			time.Sleep(300 * time.Millisecond)
			postClaim(t, c, info.ID, sA, 1, "pass", 1)
		} else {
			postClaim(t, c, info.ID, sA, 1, "pass", 1)
			postClaim(t, c, info.ID, sB, 1, "pass", 1)
		}
		time.Sleep(400 * time.Millisecond)
		ri, _ := srv.mgr.Get(info.ID)
		pass := passState(t, ri)
		if len(pass.Holders) != 1 {
			t.Fatalf("split=%v holders = %+v", split, pass.Holders)
		}
		o := outcome{}
		for idx, s := range info.Sessions {
			if s.CharID == pass.Holders[0].CharID {
				o.winnerIdx = idx
			}
		}
		for _, e := range journalEntries(t, c, info.ID) {
			if e["type"] != "tick" {
				continue
			}
			for _, raw := range e["inputs"].([]interface{}) {
				in := raw.(map[string]interface{})
				if in["cmd"] == "claim" && in["accepted"] == false {
					o.loserReason, _ = in["reason"].(string)
				}
			}
		}
		return o
	}
	together := run(false)
	split := run(true)
	if together.winnerIdx != split.winnerIdx || together.loserReason != split.loserReason {
		t.Fatalf("batching changed the verdict: together=%+v split=%+v", together, split)
	}
	if together.winnerIdx != 0 || together.loserReason != "resource-exhausted" {
		t.Fatalf("verdict = %+v, want A wins, B resource-exhausted", together)
	}
}

// A round that cannot be closed because a participant's input is missing
// is cancelled at the explicit deadline: the claim is refused with
// "round-cancelled" (re-confirmable), never granted, and the journal
// explains the cancel.
func TestRoundDeadlineCancels(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	sA := info.Sessions[0]

	postClaim(t, c, info.ID, sA, 1, "pass", 1)
	// Wait past the collection deadline.
	time.Sleep(roundWindow + 800*time.Millisecond)

	ri, _ := srv.mgr.Get(info.ID)
	pass := passState(t, ri)
	if len(pass.Holders) != 0 || pass.Remaining != 1 {
		t.Fatalf("pass granted despite unclosable round: %+v", pass)
	}
	closed := closedRound(t, ri, "pass#1")
	if closed.Status != "cancelled" || closed.CloseReason != "deadline" {
		t.Fatalf("round = %+v, want cancelled by deadline", closed)
	}
	if len(closed.Missing) != 1 || closed.Missing[0] != info.Sessions[1].CharID {
		t.Fatalf("missing = %+v, want B", closed.Missing)
	}
	found := false
	for _, e := range journalEntries(t, c, info.ID) {
		if e["type"] != "tick" {
			continue
		}
		for _, raw := range e["inputs"].([]interface{}) {
			in := raw.(map[string]interface{})
			if in["cmd"] == "claim" {
				found = true
				if in["reason"] != "round-cancelled" || in["needsReconfirm"] != true || in["roundId"] != "pass#1" {
					t.Fatalf("cancelled claim = %+v", in)
				}
			}
		}
	}
	if !found {
		t.Fatal("cancelled claim never settled in the journal")
	}
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil || !rep.Match {
		t.Fatalf("replay = %+v err=%v", rep, err)
	}
}

// An open round (with its collected claim) survives a process restart:
// the restored room keeps waiting for the missing participant and
// adjudicates under the same round identity once it arrives.
func TestOpenRoundSurvivesRestart(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	sA, sB := info.Sessions[0], info.Sessions[1]

	postClaim(t, c, info.ID, sA, 1, "pass", 1)
	time.Sleep(150 * time.Millisecond)

	mgr2 := NewManager(srv.mgr.dir)
	if err := mgr2.LoadRoom(info.ID); err != nil {
		t.Fatalf("load room: %v", err)
	}
	ri2, _ := mgr2.Get(info.ID)
	open := openRounds(t, ri2)
	if len(open) != 1 || open[0].ID != "pass#1" {
		t.Fatalf("restored open rounds = %+v", open)
	}
	if len(open[0].Claims) != 1 || open[0].Claims[0].CharID != sA.CharID {
		t.Fatalf("restored claims = %+v", open[0].Claims)
	}
	if len(open[0].WaitingFor) != 1 || open[0].WaitingFor[0] != sB.CharID {
		t.Fatalf("restored waitingFor = %+v", open[0].WaitingFor)
	}

	s2 := NewServer(srv.mgr.dir)
	s2.mgr = mgr2
	c2 := testClient{h: s2.Routes()}
	postClaim(t, c2, info.ID, sB, 1, "pass", 1)
	time.Sleep(350 * time.Millisecond)

	pass := passState(t, ri2)
	if len(pass.Holders) != 1 || pass.Holders[0].CharID != sA.CharID {
		t.Fatalf("pass after restart = %+v, want holder A", pass)
	}
	closed := closedRound(t, ri2, "pass#1")
	if closed.Status != "adjudicated" || closed.Winner != sA.CharID {
		t.Fatalf("restored round verdict = %+v", closed)
	}
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil || !rep.Match {
		t.Fatalf("replay = %+v err=%v", rep, err)
	}
}

// An explicit cancel closes the round immediately with an explainable
// result; nobody is granted the resource.
func TestCancelRoundEndpoint(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	sA := info.Sessions[0]

	postClaim(t, c, info.ID, sA, 1, "pass", 1)
	time.Sleep(150 * time.Millisecond)
	rw := c.do("POST", "/api/rooms/"+info.ID+"/rounds/pass%231/cancel", nil)
	if rw.Code != 200 {
		t.Fatalf("cancel code = %d", rw.Code)
	}
	time.Sleep(200 * time.Millisecond)

	ri, _ := srv.mgr.Get(info.ID)
	pass := passState(t, ri)
	if len(pass.Holders) != 0 || pass.Remaining != 1 {
		t.Fatalf("pass granted despite cancel: %+v", pass)
	}
	closed := closedRound(t, ri, "pass#1")
	if closed.Status != "cancelled" || closed.CloseReason != "manual" {
		t.Fatalf("round = %+v, want manual cancel", closed)
	}
	// Cancelling a non-open round is a visible error, not a silent no-op.
	rw = c.do("POST", "/api/rooms/"+info.ID+"/rounds/pass%231/cancel", nil)
	if rw.Code != 404 {
		t.Fatalf("re-cancel code = %d, want 404", rw.Code)
	}
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil || !rep.Match {
		t.Fatalf("replay = %+v err=%v", rep, err)
	}
}

// A duplicate delivery of a claim still waiting in an open round is an
// idempotent stale ack and never enters the round twice.
func TestDuplicateClaimInOpenRound(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	sA := info.Sessions[0]

	if code, _ := postClaim(t, c, info.ID, sA, 1, "pass", 1); code != 202 {
		t.Fatalf("first claim code = %d", code)
	}
	if code, res := postClaim(t, c, info.ID, sA, 1, "pass", 1); code != 200 || res.Status != "stale" {
		t.Fatalf("duplicate claim code=%d res=%+v, want stale", code, res)
	}
	time.Sleep(150 * time.Millisecond)
	ri, _ := srv.mgr.Get(info.ID)
	open := openRounds(t, ri)
	if len(open) != 1 || len(open[0].Claims) != 1 {
		t.Fatalf("duplicate claim entered the round twice: %+v", open)
	}
}
