package platform

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Simulates the front-end drill: both clients move, one stream is down and
// resumes with ?after=, a duplicate of an old input arrives late. Positions,
// acks, backlog replay and saved-room replay must all stay consistent.
func TestBlackoutResumeDuplicateAndReplay(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	s1, s2 := info.Sessions[0], info.Sessions[1]

	// Both clients move at the same time into adjacent cells.
	code, _ := postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": s1.ID, "charId": s1.CharID, "seq": 1, "cmd": "right",
	})
	if code != 202 {
		t.Fatalf("a1 code=%d", code)
	}
	code, _ = postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": s2.ID, "charId": s2.CharID, "seq": 1, "cmd": "left",
	})
	if code != 202 {
		t.Fatalf("b1 code=%d", code)
	}
	time.Sleep(300 * time.Millisecond)

	// Client A goes dark; while dark the SSE stream is gone. Client B keeps moving.
	code, _ = postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": s2.ID, "charId": s2.CharID, "seq": 2, "cmd": "left",
	})
	if code != 202 {
		t.Fatalf("b2 code=%d", code)
	}
	time.Sleep(200 * time.Millisecond)

	// A's duplicate of an already-applied input arrives after the fact: stale, no extra move.
	code, res := postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": s1.ID, "charId": s1.CharID, "seq": 1, "cmd": "right",
	})
	if code != 200 || res.Status != "stale" {
		t.Fatalf("dup code=%d res=%+v", code, res)
	}

	// A reconnects with ?after=0 and must receive every missed snapshot from the ring.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/api/rooms/"+info.ID+"/events?after=0", nil).WithContext(ctx)
	rec := newSSERecorder()
	done := make(chan struct{})
	go func() { c.h.ServeHTTP(rec, req); close(done) }()
	select {
	case <-rec.flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("backlog timeout")
	}
	body := rec.String()
	if strings.Count(body, "data:") < 2 {
		t.Fatalf("expected backlog replay of multiple snapshots, got:\n%s", body)
	}
	// The backlog must include the tick where b2 was applied (b appliedSeq=2).
	if !strings.Contains(body, fmt.Sprintf(`"seq":2`)) {
		t.Fatalf("backlog missing b seq 2:\n%s", body)
	}
	cancel()
	<-done

	// A's unacked-then-sent next input continues the clock; replay stays consistent.
	code, _ = postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": s1.ID, "charId": s1.CharID, "seq": 2, "cmd": "right",
	})
	if code != 202 {
		t.Fatalf("a2 code=%d", code)
	}
	time.Sleep(300 * time.Millisecond)
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Match || rep.FinalHash != rep.LiveFinal {
		t.Fatalf("replay mismatch after blackout drill: %+v", rep)
	}
}
