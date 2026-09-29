package platform

import (
	"testing"
	"time"
)

// After a restart (LoadRoom), the restored engine must be at the final tick:
// new inputs continue the journal instead of rewriting it, and replay still
// matches.
func TestLoadRoomRestoresAndContinues(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": info.Sessions[0].ID, "charId": info.Sessions[0].CharID,
		"seq": 1, "cmd": "right",
	})
	postInput(t, c, info.ID, map[string]interface{}{
		"sessionId": info.Sessions[1].ID, "charId": info.Sessions[1].CharID,
		"seq": 1, "cmd": "left",
	})
	time.Sleep(350 * time.Millisecond)

	// Simulate process restart with a brand-new manager over the same dir.
	mgr2 := NewManager(srv.mgr.dir)
	if err := mgr2.LoadRoom(info.ID); err != nil {
		t.Fatalf("load room: %v", err)
	}
	ri, err := mgr2.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := ri.snapshot().Tick; got < 1 {
		t.Fatalf("restored tick = %d, want >= 1", got)
	}
	finalTick := ri.snapshot().Tick

	s2 := NewServer(srv.mgr.dir)
	s2.mgr = mgr2
	c2 := testClient{h: s2.Routes()}
	// Old seq after restart is still stale; owner session is unchanged.
	code, _ := postInput(t, c2, info.ID, map[string]interface{}{
		"sessionId": info.Sessions[0].ID, "charId": info.Sessions[0].CharID,
		"seq": 1, "cmd": "right",
	})
	if code != 200 {
		t.Fatalf("old seq after restart: code=%d want 200 stale", code)
	}
	code, _ = postInput(t, c2, info.ID, map[string]interface{}{
		"sessionId": info.Sessions[0].ID, "charId": info.Sessions[0].CharID,
		"seq": 2, "cmd": "up",
	})
	if code != 202 {
		t.Fatalf("new seq after restart: code=%d want 202", code)
	}
	time.Sleep(350 * time.Millisecond)
	if got := ri.snapshot().Tick; got <= finalTick {
		t.Fatalf("tick did not advance after restore: %d <= %d", got, finalTick)
	}
	rep, err := ReplayFromDisk(srv.mgr.dir, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Match {
		t.Fatalf("replay after restart mismatch: %+v", rep)
	}
}
