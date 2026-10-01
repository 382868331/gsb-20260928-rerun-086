package platform

import (
	"encoding/json"
	"testing"
	"time"

	"gameplatform/internal/game"
	"gameplatform/internal/store"
)

func postSeq(t *testing.T, c testClient, roomID, sess, char string, seq int64, cmd game.Command) (int, EnqueueResult) {
	t.Helper()
	return postInput(t, c, roomID, map[string]interface{}{
		"sessionId": sess, "charId": char, "seq": seq, "cmd": string(cmd),
	})
}

// Delayed + reordered delivery: seq 2 lands before seq 1. Neither must be
// lost or executed out of order; once the prefix arrives both are applied in
// local order across ticks.
func TestReorderedDeliveryPreservesOrder(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	s := info.Sessions[0]

	code, _ := postSeq(t, c, info.ID, s.ID, s.CharID, 2, game.CmdRight)
	if code != 202 {
		t.Fatalf("out-of-order seq 2 admission code = %d, want 202", code)
	}
	time.Sleep(250 * time.Millisecond) // tick runs with a gap: seq 2 deferred
	code, _ = postSeq(t, c, info.ID, s.ID, s.CharID, 1, game.CmdRight)
	if code != 202 {
		t.Fatalf("late seq 1 admission code = %d, want 202", code)
	}
	time.Sleep(250 * time.Millisecond)

	ri, err := srv.mgr.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	ri.mu.Lock()
	ch := *ri.Chars[0]
	ri.mu.Unlock()
	if ch.AppliedSeq != 2 {
		t.Fatalf("appliedSeq = %d, want 2 (ordered catch-up)", ch.AppliedSeq)
	}
	if ch.X != 4 {
		t.Fatalf("x = %d, want 4 (two right moves executed in local order)", ch.X)
	}
}

// Offline burst: more than MaxAdmittedPerTick inputs in one tick. The tail is
// refused with a final ack, redelivery is stale, and replay reproduces both
// the applied and refused journal records.
func TestOfflineBurstRefusalReplaysAndSurvivesRestart(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	s := info.Sessions[0]

	total := int64(game.MaxAdmittedPerTick + 3)
	for seq := int64(1); seq <= total; seq++ {
		code, res := postSeq(t, c, info.ID, s.ID, s.CharID, seq, game.CmdRight)
		if code != 202 || res.Status != "accepted" {
			t.Fatalf("seq %d intake: code=%d res=%+v", seq, code, res)
		}
	}
	time.Sleep(350 * time.Millisecond)

	// Redelivery of a refused tail seq is an idempotent stale ack.
	code, res := postSeq(t, c, info.ID, s.ID, s.CharID, game.MaxAdmittedPerTick+1, game.CmdRight)
	if code != 200 || res.Status != "stale" {
		t.Fatalf("refused redelivery: code=%d res=%+v, want 200 stale", code, res)
	}

	// The journal must contain the final refusals alongside applied inputs.
	refused := journalRefusals(t, srv.mgr.dir, info.ID)
	if len(refused) != 3 {
		t.Fatalf("journaled refusals = %d, want 3: %+v", len(refused), refused)
	}
	for i, f := range refused {
		want := int64(game.MaxAdmittedPerTick + 1 + i)
		if f.Seq != want || f.Reason != "offline-overflow" {
			t.Fatalf("refusal %d = %+v, want seq %d offline-overflow", i, f, want)
		}
	}

	// Deterministic replay includes the refusal decisions.
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Match {
		t.Fatalf("replay mismatch after refusals: %+v", rep)
	}

	// Restart from the same journal: ack positions and refusal decisions stay
	// final and new inputs continue the clock without rewinding it.
	mgr2 := NewManager(srv.mgr.dir)
	if err := mgr2.LoadRoom(info.ID); err != nil {
		t.Fatal(err)
	}
	ri, err := mgr2.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	ri.mu.Lock()
	gotApplied := ri.Chars[0].AppliedSeq
	ri.mu.Unlock()
	if gotApplied != total {
		t.Fatalf("restored appliedSeq = %d, want %d", gotApplied, total)
	}
	tickBefore := ri.snapshot().Tick
	s2 := NewServer(srv.mgr.dir)
	s2.mgr = mgr2
	c2 := testClient{h: s2.Routes()}
	code, _ = postSeq(t, c2, info.ID, s.ID, s.CharID, total+1, game.CmdWait)
	if code != 202 {
		t.Fatalf("post-restart input code = %d, want 202", code)
	}
	time.Sleep(350 * time.Millisecond)
	if got := ri.snapshot().Tick; got <= tickBefore {
		t.Fatalf("tick did not advance after restore: %d <= %d", got, tickBefore)
	}
	rep2, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !rep2.Match {
		t.Fatalf("replay mismatch after restart+refusals: %+v", rep2)
	}
}

func journalRefusals(t *testing.T, dir, roomID string) []game.RefusedInput {
	t.Helper()
	out := []game.RefusedInput{}
	err := store.ReadEntries(dir, roomID, func(line []byte) error {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			return err
		}
		if head.Type != "tick" {
			return nil
		}
		var e struct {
			Refused []game.RefusedInput `json:"refused"`
		}
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		out = append(out, e.Refused...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
