package game

import "testing"

func testRoom() *Room {
	s1, s2 := "s1", "s2"
	chars := []*Char{
		{ID: "c1", Name: "A", OwnerSession: s1, X: 1, Y: 1, Facing: CmdRight},
		{ID: "c2", Name: "B", OwnerSession: s2, X: 3, Y: 1, Facing: CmdLeft},
	}
	cfg := Config{Width: 6, Height: 4, Obstacles: [][2]int{{2, 0}}}
	return NewRoom("r", cfg, chars)
}

// A late or retried input (same or lower seq) must never execute twice.
func TestStaleInputNotExecutedTwice(t *testing.T) {
	r := testRoom()
	first := Input{SessionID: "s1", CharID: "c1", Seq: 1, Cmd: CmdRight}
	applied, _ := r.ApplyTick([]Input{first})
	if len(applied) != 1 || !applied[0].Accepted {
		t.Fatalf("first input should move: %+v", applied)
	}
	if got := r.Chars[0].X; got != 2 {
		t.Fatalf("x after first move = %d, want 2", got)
	}
	// Duplicate delivered in a later tick: ignored entirely.
	applied, _ = r.ApplyTick([]Input{first})
	if len(applied) != 0 {
		t.Fatalf("duplicate must not be applied, got %+v", applied)
	}
	if got := r.Chars[0].X; got != 2 {
		t.Fatalf("duplicate moved character: x = %d", got)
	}
	if !r.IsStale(first) {
		t.Fatal("duplicate input should be classified stale")
	}
}

// A session may only drive the character it owns; reconnecting with another
// session's id cannot take over the character.
func TestForeignSessionCannotDrive(t *testing.T) {
	r := testRoom()
	hijack := Input{SessionID: "attacker", CharID: "c1", Seq: 1, Cmd: CmdUp}
	applied, _ := r.ApplyTick([]Input{hijack})
	if len(applied) != 0 {
		t.Fatalf("foreign session input must be refused, got %+v", applied)
	}
	// Also: the real owner cannot drive the other character.
	r2 := testRoom()
	cross := Input{SessionID: "s1", CharID: "c2", Seq: 1, Cmd: CmdUp}
	applied, _ = r2.ApplyTick([]Input{cross})
	if len(applied) != 0 {
		t.Fatalf("cross-character input must be refused, got %+v", applied)
	}
}

// The same inputs replayed from scratch must reproduce identical hashes.
func TestReplayDeterministic(t *testing.T) {
	r := testRoom()
	stream := []Input{
		{SessionID: "s1", CharID: "c1", Seq: 1, Cmd: CmdRight},
		{SessionID: "s2", CharID: "c2", Seq: 1, Cmd: CmdLeft},
		{SessionID: "s1", CharID: "c1", Seq: 2, Cmd: CmdDown},
		{SessionID: "s2", CharID: "c2", Seq: 2, Cmd: CmdUp},
		{SessionID: "s1", CharID: "c1", Seq: 3, Cmd: CmdLeft},
		{SessionID: "s2", CharID: "c2", Seq: 3, Cmd: CmdRight},
		// blocked by wall at (2,0)
		{SessionID: "s1", CharID: "c1", Seq: 4, Cmd: CmdUp},
	}
	// batch inputs across ticks
	batches := [][]Input{{stream[0], stream[1]}, {stream[2], stream[3]}, {stream[4], stream[5]}, {stream[6]}}
	records := make([]TickRecord, 0)
	for _, b := range batches {
		applied, snap := r.ApplyTick(b)
		snap.Hash = snap.HashValue()
		records = append(records, TickRecord{Type: "tick", Tick: snap.Tick, Inputs: applied})
	}
	chars2 := []*Char{
		{ID: "c1", Name: "A", OwnerSession: "s1", X: 1, Y: 1, Facing: CmdRight},
		{ID: "c2", Name: "B", OwnerSession: "s2", X: 3, Y: 1, Facing: CmdLeft},
	}
	snaps, err := Replay("r", Config{Width: 6, Height: 4, Obstacles: [][2]int{{2, 0}}}, chars2, records)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(snaps) != len(records) {
		t.Fatalf("replayed %d ticks, want %d", len(snaps), len(records))
	}
	if snaps[len(snaps)-1].Hash != r.Snapshot().HashValue() {
		t.Fatalf("replay final hash mismatch\nlive: %s\nreplay: %s",
			r.Snapshot().HashValue(), snaps[len(snaps)-1].Hash)
	}
}

// Two characters cannot occupy the same cell, and moving into a wall is
// refused; blocked moves still ack the seq (it is consumed, not retried).
func TestCollisionConsumesSeq(t *testing.T) {
	r := testRoom()
	// c2 at (3,1) moves left to (2,1); c1 at (1,1) moves right to (2,1).
	// c1 runs first in declaration order and occupies (2,1), so c2 is blocked.
	applied, _ := r.ApplyTick([]Input{
		{SessionID: "s1", CharID: "c1", Seq: 1, Cmd: CmdRight},
		{SessionID: "s2", CharID: "c2", Seq: 1, Cmd: CmdLeft},
	})
	if !applied[0].Accepted || applied[1].Accepted {
		t.Fatalf("want accepted=true,false got %v,%v", applied[0].Accepted, applied[1].Accepted)
	}
	if r.Chars[0].AppliedSeq != 1 || r.Chars[1].AppliedSeq != 1 {
		t.Fatal("blocked input must still consume seq")
	}
}
