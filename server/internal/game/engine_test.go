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
	applied, refused, deferred, _ := r.ApplyTick([]Input{first})
	if len(applied) != 1 || !applied[0].Accepted {
		t.Fatalf("first input should move: %+v", applied)
	}
	if len(refused) != 0 || len(deferred) != 0 {
		t.Fatalf("first tick unexpected refused=%v deferred=%v", refused, deferred)
	}
	if got := r.Chars[0].X; got != 2 {
		t.Fatalf("x after first move = %d, want 2", got)
	}
	// Duplicate delivered in a later tick: ignored entirely.
	applied, refused, deferred, _ = r.ApplyTick([]Input{first})
	if len(applied) != 0 {
		t.Fatalf("duplicate must not be applied, got %+v", applied)
	}
	if len(refused) != 0 || len(deferred) != 0 {
		t.Fatalf("duplicate must not be refused or deferred: %v %v", refused, deferred)
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
	applied, _, _, _ := r.ApplyTick([]Input{hijack})
	if len(applied) != 0 {
		t.Fatalf("foreign session input must be refused, got %+v", applied)
	}
	// Also: the real owner cannot drive the other character.
	r2 := testRoom()
	cross := Input{SessionID: "s1", CharID: "c2", Seq: 1, Cmd: CmdUp}
	applied, _, _, _ = r2.ApplyTick([]Input{cross})
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
		applied, refused, deferred, snap := r.ApplyTick(b)
		if len(refused) != 0 || len(deferred) != 0 {
			t.Fatalf("unexpected refused/deferred in replay setup: %v %v", refused, deferred)
		}
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
	applied, _, _, _ := r.ApplyTick([]Input{
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

// Out-of-order delivery must not execute inputs out of their local order: a
// seq that arrives ahead of its missing prefix is deferred across ticks and
// applied (still in order) once the prefix catches up.
func TestOutOfOrderDeferredThenAppliedInOrder(t *testing.T) {
	r := testRoom()
	// seq 2 arrives first (delayed seq 1 still in flight).
	applied, refused, deferred, _ := r.ApplyTick([]Input{
		{SessionID: "s1", CharID: "c1", Seq: 2, Cmd: CmdRight},
	})
	if len(applied) != 0 || len(refused) != 0 || len(deferred) != 1 {
		t.Fatalf("seq 2 should defer: applied=%v refused=%v deferred=%v", applied, refused, deferred)
	}
	if r.Chars[0].AppliedSeq != 0 {
		t.Fatalf("deferred input must not advance ack: %d", r.Chars[0].AppliedSeq)
	}
	// Next tick the delayed seq 1 arrives; feed the retained tail back first.
	applied, refused, deferred, _ = r.ApplyTick(append([]Input{
		{SessionID: "s1", CharID: "c1", Seq: 1, Cmd: CmdRight},
	}, deferred...))
	if len(applied) != 2 || len(refused) != 0 || len(deferred) != 0 {
		t.Fatalf("both should apply in order: applied=%v refused=%v deferred=%v", applied, refused, deferred)
	}
	if applied[0].Seq != 1 || applied[1].Seq != 2 {
		t.Fatalf("order violated: %d then %d", applied[0].Seq, applied[1].Seq)
	}
	// seq 2 moves toward (3,1) which c2 occupies, so it is applied-in-order
	// but blocked: both seqs consumed, position stays (2,1).
	if r.Chars[0].X != 2 || applied[1].Accepted {
		t.Fatalf("ordered collision result = x %d accepted %v, want x2 blocked", r.Chars[0].X, applied[1].Accepted)
	}
}

// An offline burst larger than MaxAdmittedPerTick has its tail refused in the
// same tick. Refused seqs are still acked, so redelivery is stale and can
// never mutate state again; replay reproduces the refusals deterministically.
func TestOfflineBurstTailRefusedAndAcked(t *testing.T) {
	r := testRoom()
	in := make([]Input, 0, MaxAdmittedPerTick+2)
	for seq := int64(1); seq <= int64(MaxAdmittedPerTick+2); seq++ {
		in = append(in, Input{SessionID: "s1", CharID: "c1", Seq: seq, Cmd: CmdRight})
	}
	applied, refused, deferred, _ := r.ApplyTick(in)
	if len(applied) != MaxAdmittedPerTick || len(refused) != 2 || len(deferred) != 0 {
		t.Fatalf("want %d applied / 2 refused / 0 deferred, got %d / %d / %d",
			MaxAdmittedPerTick, len(applied), len(refused), len(deferred))
	}
	for i, f := range refused {
		wantSeq := int64(MaxAdmittedPerTick + 1 + i)
		if f.Seq != wantSeq || f.Reason != "offline-overflow" {
			t.Fatalf("refused[%d] = %+v, want seq %d offline-overflow", i, f, wantSeq)
		}
	}
	if r.Chars[0].AppliedSeq != int64(MaxAdmittedPerTick+2) {
		t.Fatalf("refused seqs must still ack: appliedSeq=%d", r.Chars[0].AppliedSeq)
	}
	// Redelivery of a refused input is stale and changes nothing.
	x := r.Chars[0].X
	applied, refused, deferred, _ = r.ApplyTick([]Input{
		{SessionID: "s1", CharID: "c1", Seq: int64(MaxAdmittedPerTick + 1), Cmd: CmdRight},
	})
	if len(applied) != 0 || len(refused) != 0 || len(deferred) != 0 || r.Chars[0].X != x {
		t.Fatalf("refused input redelivery must be inert: applied=%v refused=%v", applied, refused)
	}
	if !r.IsStale(Input{CharID: "c1", Seq: int64(MaxAdmittedPerTick + 1)}) {
		t.Fatal("refused seq must classify stale on redelivery")
	}

}

// A jump beyond the unacked window is refused as offline-window so a lost
// prefix cannot stall the session forever.
func TestGapBeyondWindowRefused(t *testing.T) {
	r := testRoom()
	bogus := Input{SessionID: "s1", CharID: "c1", Seq: int64(MaxPendingGap) + 2, Cmd: CmdRight}
	applied, refused, deferred, _ := r.ApplyTick([]Input{bogus})
	if len(applied) != 0 || len(refused) != 1 || len(deferred) != 0 {
		t.Fatalf("far jump should be refused: applied=%v refused=%v deferred=%v", applied, refused, deferred)
	}
	if refused[0].Reason != "offline-window" || refused[0].Seq != bogus.Seq {
		t.Fatalf("bad refusal: %+v", refused[0])
	}
	if r.Chars[0].AppliedSeq != bogus.Seq {
		t.Fatal("refused far jump must still ack the seq")
	}
}

// Refusal decisions are part of the journal: replay rebuilds identical applied
// and refused lists and therefore the identical authoritative final state.
func TestReplayReconstructsRefusals(t *testing.T) {
	r := testRoom()
	in := make([]Input, 0, MaxAdmittedPerTick+1)
	for seq := int64(1); seq <= int64(MaxAdmittedPerTick+1); seq++ {
		in = append(in, Input{SessionID: "s1", CharID: "c1", Seq: seq, Cmd: CmdRight})
	}
	applied, refused, _, snap := r.ApplyTick(in)
	snap.Hash = snap.HashValue()
	rec := TickRecord{Type: "tick", Tick: 1, Inputs: applied, Refused: refused}
	chars2 := []*Char{
		{ID: "c1", Name: "A", OwnerSession: "s1", X: 1, Y: 1, Facing: CmdRight},
		{ID: "c2", Name: "B", OwnerSession: "s2", X: 3, Y: 1, Facing: CmdLeft},
	}
	snaps, err := Replay("r", Config{Width: 6, Height: 4, Obstacles: [][2]int{{2, 0}}}, chars2, []TickRecord{rec})
	if err != nil {
		t.Fatalf("replay with refusals: %v", err)
	}
	if snaps[0].Hash != snap.Hash {
		t.Fatalf("refusal replay hash mismatch\nlive %s\nrep  %s", snap.Hash, snaps[0].Hash)
	}
}
