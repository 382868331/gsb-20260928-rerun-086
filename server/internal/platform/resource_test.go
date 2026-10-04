package platform

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"gameplatform/internal/game"
)

func postClaim(t *testing.T, c testClient, roomID string, s SessionView, seq int64, resource string, rv int) (int, EnqueueResult) {
	t.Helper()
	return postInput(t, c, roomID, map[string]interface{}{
		"sessionId": s.ID, "charId": s.CharID, "seq": seq, "cmd": "claim",
		"resource": resource, "ruleVersion": rv,
	})
}

func passState(t *testing.T, ri *RoomInstance) game.ResourceState {
	t.Helper()
	for _, rs := range ri.snapshot().Resources {
		if rs.ID == "pass" {
			return rs
		}
	}
	t.Fatal("pass resource missing from snapshot")
	return game.ResourceState{}
}

// Two clients contend for one pass slot. The winner is fixed by
// deterministic arbitration, the loser is rejected with an explicit
// reason, duplicates/late acks never re-grant, and the outcome survives a
// restart with replay matching.
func TestContendedClaimsAcrossClients(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	sA, sB := info.Sessions[0], info.Sessions[1]

	// B's claim arrives first; A still wins by deterministic order.
	if code, _ := postClaim(t, c, info.ID, sB, 1, "pass", 1); code != 202 {
		t.Fatalf("B claim code=%d", code)
	}
	if code, _ := postClaim(t, c, info.ID, sA, 1, "pass", 1); code != 202 {
		t.Fatalf("A claim code=%d", code)
	}
	// The contention round collects for RoundCollectTicks ticks before
	// adjudicating at its boundary.
	time.Sleep(600 * time.Millisecond)

	ri, err := srv.mgr.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	pass := passState(t, ri)
	if pass.Remaining != 0 || len(pass.Holders) != 1 || pass.Holders[0].CharID != sA.CharID {
		t.Fatalf("pass = %+v, want single holder A", pass)
	}

	// Late duplicate of the winning claim: stale ack, no second grant.
	if code, res := postClaim(t, c, info.ID, sA, 1, "pass", 1); code != 200 || res.Status != "stale" {
		t.Fatalf("duplicate A claim code=%d res=%+v", code, res)
	}
	// B's dependent action (rely on its rejected claim) gets its own
	// independent reason; B's independent action is still processed.
	postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": sB.ID, "charId": sB.CharID, "seq": 2, "cmd": "up", "dependsOn": 1, "ruleVersion": 1,
	})
	postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": sB.ID, "charId": sB.CharID, "seq": 3, "cmd": "left", "ruleVersion": 1,
	})
	time.Sleep(600 * time.Millisecond)

	pass = passState(t, ri)
	if len(pass.Holders) != 1 {
		t.Fatalf("duplicate delivery re-granted: %+v", pass)
	}
	var depReason, indepAccepted string
	var journal []map[string]interface{}
	rw := c.do("GET", "/api/rooms/"+info.ID+"/journal", nil)
	for _, line := range bytes.Split(rw.Body.Bytes(), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var e map[string]interface{}
		if json.Unmarshal(line, &e) == nil {
			journal = append(journal, e)
		}
	}
	for _, e := range journal {
		if e["type"] != "tick" {
			continue
		}
		for _, raw := range e["inputs"].([]interface{}) {
			in := raw.(map[string]interface{})
			if in["charId"] == sB.CharID && in["seq"] == float64(2) {
				depReason, _ = in["reason"].(string)
			}
			if in["charId"] == sB.CharID && in["seq"] == float64(3) && in["accepted"] == true {
				indepAccepted = "yes"
			}
		}
	}
	if depReason != "dependency-failed" {
		t.Fatalf("B seq2 reason = %q, want dependency-failed", depReason)
	}
	if indepAccepted != "yes" {
		t.Fatal("B seq3 independent action was not processed")
	}

	// Restart: grants survive, replay matches, old claims stay stale.
	mgr2 := NewManager(srv.mgr.dir)
	if err := mgr2.LoadRoom(info.ID); err != nil {
		t.Fatalf("load room: %v", err)
	}
	ri2, _ := mgr2.Get(info.ID)
	pass = passState(t, ri2)
	if pass.Remaining != 0 || len(pass.Holders) != 1 || pass.Holders[0].CharID != sA.CharID {
		t.Fatalf("restored pass = %+v", pass)
	}
	s2 := NewServer(srv.mgr.dir)
	s2.mgr = mgr2
	c2 := testClient{h: s2.Routes()}
	if code, res := postClaim(t, c2, info.ID, sA, 1, "pass", 1); code != 200 || res.Status != "stale" {
		t.Fatalf("claim after restart code=%d res=%+v", code, res)
	}
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil || !rep.Match {
		t.Fatalf("replay = %+v err=%v", rep, err)
	}
}

// The same two logical claims, split into different arrival batches
// (different ticks), must yield the same winner: the contention round's
// collection boundary and stable order decide, not network batching.
func TestClaimArbitrationIndependentOfBatching(t *testing.T) {
	winners := map[int]string{}
	for i := 0; i < 2; i++ {
		c, srv := newTestServer(t)
		info := createRoom(t, c)
		sA, sB := info.Sessions[0], info.Sessions[1]
		first, second := sA, sB
		if i == 1 {
			first, second = sB, sA
		}
		postClaim(t, c, info.ID, first, 1, "pass", 1)
		time.Sleep(150 * time.Millisecond) // force a separate batch/tick
		postClaim(t, c, info.ID, second, 1, "pass", 1)
		time.Sleep(700 * time.Millisecond)
		ri, _ := srv.mgr.Get(info.ID)
		pass := passState(t, ri)
		if len(pass.Holders) != 1 {
			t.Fatalf("run %d: holders = %+v", i, pass.Holders)
		}
		for idx, s := range info.Sessions {
			if s.CharID == pass.Holders[0].CharID {
				winners[i] = string(rune('A' + idx))
			}
		}
		if pass.Holders[0].Round != "pass-r1" {
			t.Fatalf("run %d: grant does not name its round: %+v", i, pass.Holders[0])
		}
		rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
		if err != nil || !rep.Match {
			t.Fatalf("run %d replay = %+v err=%v", i, rep, err)
		}
	}
	if winners[0] != "A" || winners[1] != "A" {
		t.Fatalf("batching changed the winner: %v", winners)
	}
}

// A round cannot close while a client's claim is stuck behind a missing
// earlier input: it waits with an explainable reason, then adjudicates
// the late-collected claim in the same round.
func TestContentionRoundWaitsForMissingInput(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	sA, sB := info.Sessions[0], info.Sessions[1]

	postClaim(t, c, info.ID, sA, 1, "pass", 1)
	// B's claim is stuck behind the missing seq 1/2 (offline-formed).
	postClaim(t, c, info.ID, sB, 3, "pass", 1)
	time.Sleep(450 * time.Millisecond)

	ri, _ := srv.mgr.Get(info.ID)
	rounds := ri.snapshot().Rounds
	if len(rounds) != 1 || rounds[0].State != "waiting" || rounds[0].Reason == "" {
		t.Fatalf("round should be waiting with a reason: %+v", rounds)
	}
	if pass := passState(t, ri); len(pass.Holders) != 0 {
		t.Fatalf("round closed despite the missing input: %+v", pass)
	}

	// The missing inputs arrive; B's claim joins the still-open round.
	postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": sB.ID, "charId": sB.CharID, "seq": 1, "cmd": "wait", "ruleVersion": 1,
	})
	postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": sB.ID, "charId": sB.CharID, "seq": 2, "cmd": "wait", "ruleVersion": 1,
	})
	time.Sleep(900 * time.Millisecond)

	pass := passState(t, ri)
	if len(pass.Holders) != 1 || pass.Holders[0].CharID != sA.CharID {
		t.Fatalf("pass = %+v, want single holder A", pass)
	}
	// B's late-collected claim was adjudicated in the SAME round and
	// refused with its own explicit reason.
	var waitingSeen, bVerdict string
	rw := c.do("GET", "/api/rooms/"+info.ID+"/journal", nil)
	for _, line := range bytes.Split(rw.Body.Bytes(), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var e map[string]interface{}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if e["type"] == "round" && e["state"] == "waiting" {
			waitingSeen, _ = e["reason"].(string)
		}
		if e["type"] == "tick" {
			for _, raw := range e["inputs"].([]interface{}) {
				in := raw.(map[string]interface{})
				if in["charId"] == sB.CharID && in["seq"] == float64(3) && in["derived"] == true {
					bVerdict, _ = in["reason"].(string)
					if in["roundId"] != pass.Holders[0].Round {
						t.Fatalf("B verdict in different round: %+v", in)
					}
				}
			}
		}
	}
	if waitingSeen == "" {
		t.Fatal("no journaled waiting event for the postponed boundary")
	}
	if bVerdict != "resource-exhausted" {
		t.Fatalf("B late claim verdict = %q, want resource-exhausted", bVerdict)
	}
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil || !rep.Match {
		t.Fatalf("replay = %+v err=%v", rep, err)
	}
}

// If the missing input never arrives, the round stops waiting after the
// bounded postponements and closes with the missing claimant explicitly
// cancelled — never silently decided by arrival order.
func TestContentionRoundCancelsMissingClaimant(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	sA, sB := info.Sessions[0], info.Sessions[1]

	postClaim(t, c, info.ID, sA, 1, "pass", 1)
	postClaim(t, c, info.ID, sB, 3, "pass", 1) // stuck behind missing seq 1/2
	// Boundary + MaxRoundPostponements * RoundWaitExtend ticks of waiting.
	time.Sleep(2600 * time.Millisecond)

	ri, _ := srv.mgr.Get(info.ID)
	pass := passState(t, ri)
	if len(pass.Holders) != 1 || pass.Holders[0].CharID != sA.CharID {
		t.Fatalf("pass = %+v, want single holder A", pass)
	}
	if rounds := ri.snapshot().Rounds; len(rounds) != 0 {
		t.Fatalf("round still open after bounded waiting: %+v", rounds)
	}
	var cancelled bool
	rw := c.do("GET", "/api/rooms/"+info.ID+"/journal", nil)
	for _, line := range bytes.Split(rw.Body.Bytes(), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var e map[string]interface{}
		if json.Unmarshal(line, &e) != nil || e["type"] != "round" || e["state"] != "closed" {
			continue
		}
		for _, raw := range e["cancelled"].([]interface{}) {
			cl := raw.(map[string]interface{})
			if cl["charId"] == sB.CharID && cl["seq"] == float64(3) {
				cancelled = true
			}
		}
	}
	if !cancelled {
		t.Fatal("missing claimant was not explicitly cancelled in the journal")
	}

	// When B's inputs finally arrive, the cancelled claim joins a fresh
	// round and receives its own explicit verdict.
	postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": sB.ID, "charId": sB.CharID, "seq": 1, "cmd": "wait", "ruleVersion": 1,
	})
	postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": sB.ID, "charId": sB.CharID, "seq": 2, "cmd": "wait", "ruleVersion": 1,
	})
	time.Sleep(700 * time.Millisecond)
	pass = passState(t, ri)
	if len(pass.Holders) != 1 || pass.Holders[0].CharID != sA.CharID {
		t.Fatalf("late arrival re-granted the resource: %+v", pass)
	}
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil || !rep.Match {
		t.Fatalf("replay = %+v err=%v", rep, err)
	}
}

// A room restarted while a contention round is still open resumes the
// same round (identity, boundary, collected claims) and adjudicates it
// exactly once.
func TestContentionRoundSurvivesRestart(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	sA, sB := info.Sessions[0], info.Sessions[1]

	postClaim(t, c, info.ID, sA, 1, "pass", 1)
	postClaim(t, c, info.ID, sB, 1, "pass", 1)
	time.Sleep(250 * time.Millisecond) // collected, round open, not yet closed

	ri, _ := srv.mgr.Get(info.ID)
	if rounds := ri.snapshot().Rounds; len(rounds) != 1 {
		t.Fatalf("expected one open round before restart, got %+v", rounds)
	}
	// Stop the old room's tick loop, then restore from the journal.
	ri.mu.Lock()
	ri.stopped = true
	ri.mu.Unlock()
	time.Sleep(250 * time.Millisecond)

	mgr2 := NewManager(srv.mgr.dir)
	if err := mgr2.LoadRoom(info.ID); err != nil {
		t.Fatalf("load room: %v", err)
	}
	ri2, _ := mgr2.Get(info.ID)
	time.Sleep(700 * time.Millisecond)
	pass := passState(t, ri2)
	if len(pass.Holders) != 1 || pass.Holders[0].CharID != sA.CharID {
		t.Fatalf("restored round adjudicated differently: %+v", pass)
	}
	if pass.Holders[0].Round != "pass-r1" {
		t.Fatalf("grant lost its round identity across restart: %+v", pass.Holders[0])
	}
	if rounds := ri2.snapshot().Rounds; len(rounds) != 0 {
		t.Fatalf("round still open after adjudication: %+v", rounds)
	}
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil || !rep.Match {
		t.Fatalf("replay = %+v err=%v", rep, err)
	}
}

// The same contention, submitted in the opposite arrival order on a fresh
// room, yields the identical arbitration outcome.
func TestClaimArbitrationIndependentOfSubmissionOrder(t *testing.T) {
	winners := map[int]string{}
	for i := 0; i < 2; i++ {
		c, srv := newTestServer(t)
		info := createRoom(t, c)
		sA, sB := info.Sessions[0], info.Sessions[1]
		if i == 0 {
			postClaim(t, c, info.ID, sA, 1, "pass", 1)
			postClaim(t, c, info.ID, sB, 1, "pass", 1)
		} else {
			postClaim(t, c, info.ID, sB, 1, "pass", 1)
			postClaim(t, c, info.ID, sA, 1, "pass", 1)
		}
		time.Sleep(350 * time.Millisecond)
		ri, _ := srv.mgr.Get(info.ID)
		pass := passState(t, ri)
		if len(pass.Holders) != 1 {
			t.Fatalf("run %d: holders = %+v", i, pass.Holders)
		}
		// Char IDs differ per room; compare by declaration index.
		for idx, s := range info.Sessions {
			if s.CharID == pass.Holders[0].CharID {
				winners[i] = string(rune('A' + idx))
			}
		}
	}
	if winners[0] != winners[1] || winners[0] != "A" {
		t.Fatalf("arbitration depended on submission order: %v", winners)
	}
}

// A session saved with unconfirmed claims reopens: the queued claims are
// restored and settled under their original rule basis, and a rule switch
// in between marks them needsReconfirm instead of rewriting them.
func TestQueuedClaimsRestoredAndRuleSwitch(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	sA, sB := info.Sessions[0], info.Sessions[1]

	// Both clients form claims while "offline": queue seq 2 without seq 1
	// so nothing executes before the restart.
	postClaim(t, c, info.ID, sA, 2, "pass", 1)
	postClaim(t, c, info.ID, sB, 2, "pass", 1)

	mgr2 := NewManager(srv.mgr.dir)
	if err := mgr2.LoadRoom(info.ID); err != nil {
		t.Fatalf("load room: %v", err)
	}
	s2 := NewServer(srv.mgr.dir)
	s2.mgr = mgr2
	c2 := testClient{h: s2.Routes()}

	// Rules switch while the claims are still in flight.
	rw := c2.do("POST", "/api/rooms/"+info.ID+"/rules", bytes.NewReader([]byte(
		`{"sealed":[],"claimsClosed":true}`)))
	if rw.Code != 200 {
		t.Fatalf("rules switch code=%d", rw.Code)
	}

	// The missing predecessors reconnect the queues; both claims then
	// settle under the new rules with their original basis preserved.
	postInput(t, c2, info.ID, map[string]interface{}{
		"sessionId": sA.ID, "charId": sA.CharID, "seq": 1, "cmd": "wait", "ruleVersion": 1,
	})
	postInput(t, c2, info.ID, map[string]interface{}{
		"sessionId": sB.ID, "charId": sB.CharID, "seq": 1, "cmd": "wait", "ruleVersion": 1,
	})
	time.Sleep(600 * time.Millisecond)

	ri2, _ := mgr2.Get(info.ID)
	pass := passState(t, ri2)
	if len(pass.Holders) != 0 || pass.Remaining != 1 {
		t.Fatalf("pass granted despite closed claims: %+v", pass)
	}
	var reconfirms int
	rw = c2.do("GET", "/api/rooms/"+info.ID+"/journal", nil)
	for _, line := range bytes.Split(rw.Body.Bytes(), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var e map[string]interface{}
		if json.Unmarshal(line, &e) != nil || e["type"] != "tick" {
			continue
		}
		for _, raw := range e["inputs"].([]interface{}) {
			in := raw.(map[string]interface{})
			// Only the round's verdict records (derived) carry the
			// adjudication; collection records are still undecided.
			if in["cmd"] == "claim" && in["derived"] == true {
				if in["reason"] != "sealed" || in["needsReconfirm"] != true || in["ruleVersion"] != float64(1) {
					t.Fatalf("restored claim judged = %+v", in)
				}
				reconfirms++
			}
		}
	}
	if reconfirms != 2 {
		t.Fatalf("needsReconfirm claims = %d, want 2", reconfirms)
	}
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil || !rep.Match {
		t.Fatalf("replay = %+v err=%v", rep, err)
	}
}
