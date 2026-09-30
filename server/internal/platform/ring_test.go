package platform

import (
	"encoding/json"
	"strings"
	"testing"

	"gameplatform/internal/game"
)

func TestRingTrimsToCapacity(t *testing.T) {
	ri := &RoomInstance{subs: map[int]chan pubMsg{}}
	for i := 0; i < ringCapacity+50; i++ {
		ri.publishLocked(pubMsg{id: int64(i), payload: []byte("x")})
	}
	if len(ri.ring) != ringCapacity {
		t.Fatalf("ring length = %d, want %d", len(ri.ring), ringCapacity)
	}
	if ri.ring[0].id != 50 {
		t.Fatalf("oldest retained id = %d, want 50", ri.ring[0].id)
	}
}

func TestHeartbeatEventSerializesAppliedAsEmptyArray(t *testing.T) {
	ev := snapshotEvent{Type: "snapshot", Applied: []game.AppliedInput{}}
	out, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"applied":[]`) {
		t.Fatalf("heartbeat must carry applied as empty array, got %s", out)
	}
}
