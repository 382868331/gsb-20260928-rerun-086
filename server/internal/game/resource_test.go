package game

import "testing"

// resourceRoom starts a room with exactly one shared pass slot.
func resourceRoom() *Room {
	chars := []*Char{
		{ID: "c1", Name: "A", OwnerSession: "s1", X: 1, Y: 1, Facing: CmdRight},
		{ID: "c2", Name: "B", OwnerSession: "s2", X: 3, Y: 1, Facing: CmdLeft},
	}
	cfg := Config{Width: 6, Height: 4, Passes: 1, Obstacles: [][2]int{{2, 0}}}
	return NewRoom("r", cfg, chars)
}

// Two clients claim the single shared slot in the same tick: the character
// earlier in declaration order wins, the other is rejected with an explicit
// conflict reason. The last prediction to arrive is NOT the outcome.
func TestSharedResourceContentionSameTick(t *testing.T) {
	r := resourceRoom()
	// c2's input is listed first (as if it arrived first); adjudication must
	// still follow the predetermined (char order, seq) rule.
	applied, snap := r.ApplyTick([]Input{
		{SessionID: "s2", CharID: "c2", Seq: 1, Cmd: CmdClaim},
		{SessionID: "s1", CharID: "c1", Seq: 1, Cmd: CmdClaim},
	})
	if len(applied) != 2 {
		t.Fatalf("applied = %d, want 2", len(applied))
	}
	if applied[0].CharID != "c1" || !applied[0].Accepted {
		t.Fatalf("first adjudicated must be c1 granted: %+v", applied[0])
	}
	if applied[1].CharID != "c2" || applied[1].Accepted || applied[1].Reason != "resource-exhausted" {
		t.Fatalf("c2 must be rejected with resource-exhausted: %+v", applied[1])
	}
	if snap.PassesLeft != 0 || snap.Chars[0].Passes != 1 || snap.Chars[1].Passes != 0 {
		t.Fatalf("snapshot resource state wrong: %+v", snap)
	}
}

// A granted slot can never be granted twice: late duplicates of the winning
// input are dropped as stale, and later claims by the other client are
// rejected even across ticks.
func TestSharedResourceNotGrantedTwice(t *testing.T) {
	r := resourceRoom()
	winner := Input{SessionID: "s1", CharID: "c1", Seq: 1, Cmd: CmdClaim}
	applied, _ := r.ApplyTick([]Input{winner})
	if !applied[0].Accepted || r.PassesLeft != 0 {
		t.Fatalf("first claim must be granted: %+v left=%d", applied[0], r.PassesLeft)
	}
	// Late duplicate of the same claim: ignored entirely, no second grant.
	applied, _ = r.ApplyTick([]Input{winner})
	if len(applied) != 0 || r.Chars[0].Passes != 1 {
		t.Fatalf("duplicate claim must not re-grant: %+v passes=%d", applied, r.Chars[0].Passes)
	}
	// The other client's claim in a later tick is rejected: pool exhausted.
	applied, _ = r.ApplyTick([]Input{{SessionID: "s2", CharID: "c2", Seq: 1, Cmd: CmdClaim}})
	if len(applied) != 1 || applied[0].Accepted || applied[0].Reason != "resource-exhausted" {
		t.Fatalf("late claim by c2 must be rejected: %+v", applied)
	}
	if r.PassesLeft != 0 || r.Chars[0].Passes != 1 || r.Chars[1].Passes != 0 {
		t.Fatalf("resource state corrupted: left=%d c1=%d c2=%d", r.PassesLeft, r.Chars[0].Passes, r.Chars[1].Passes)
	}
}

// After a cross-client conflict rejection, independent actions of the same
// session keep processing, while an action that declared a dependency on
// the rejected one gets its own explicit reason.
func TestDependentVsIndependentActions(t *testing.T) {
	r := resourceRoom()
	// Tick 1: c1 takes the only slot; c2's claim conflicts and is rejected.
	r.ApplyTick([]Input{
		{SessionID: "s1", CharID: "c1", Seq: 1, Cmd: CmdClaim},
		{SessionID: "s2", CharID: "c2", Seq: 1, Cmd: CmdClaim},
	})
	// Tick 2: c2 issues a dependent move (depends on the rejected claim)
	// and an independent move. The dependent one is rejected with its own
	// reason; the independent one still executes.
	applied, _ := r.ApplyTick([]Input{
		{SessionID: "s2", CharID: "c2", Seq: 2, Cmd: CmdUp, DependsOn: 1},
		{SessionID: "s2", CharID: "c2", Seq: 3, Cmd: CmdLeft},
	})
	if len(applied) != 2 {
		t.Fatalf("applied = %d, want 2", len(applied))
	}
	if applied[0].Accepted || applied[0].Reason != "dependency-rejected" {
		t.Fatalf("dependent input must be rejected with dependency-rejected: %+v", applied[0])
	}
	if !applied[1].Accepted {
		t.Fatalf("independent input must still execute: %+v", applied[1])
	}
	if r.Chars[1].X != 2 || r.Chars[1].Y != 1 {
		t.Fatalf("independent move must apply: (%d,%d), want (2,1)", r.Chars[1].X, r.Chars[1].Y)
	}
	// A dependency on a successful action proceeds normally.
	applied, _ = r.ApplyTick([]Input{
		{SessionID: "s1", CharID: "c1", Seq: 2, Cmd: CmdUp, DependsOn: 1},
	})
	if !applied[0].Accepted {
		t.Fatalf("dependency on accepted action must proceed: %+v", applied[0])
	}
}

// A rule switch that closes claims rejects old-basis claims with an
// explicit re-confirm request; the original basis is preserved.
func TestClaimsClosedNeedsReconfirm(t *testing.T) {
	r := resourceRoom()
	rules := DefaultRules()
	rules.Version = 2
	rules.ClaimsClosed = true
	r.SetRules(rules)

	applied, _ := r.ApplyTick([]Input{
		{SessionID: "s1", CharID: "c1", Seq: 1, Cmd: CmdClaim, RuleVersion: 1}, // formed under v1
		{SessionID: "s2", CharID: "c2", Seq: 1, Cmd: CmdClaim, RuleVersion: 2}, // formed under v2
	})
	if applied[0].Accepted || applied[0].Reason != "claims-closed" || !applied[0].NeedsReconfirm {
		t.Fatalf("old-basis claim must be claims-closed + reconfirm: %+v", applied[0])
	}
	if applied[1].Accepted || applied[1].Reason != "claims-closed" || applied[1].NeedsReconfirm {
		t.Fatalf("current-basis claim must be claims-closed without reconfirm: %+v", applied[1])
	}
	if r.PassesLeft != 1 {
		t.Fatalf("closed claims must not touch the pool: left=%d", r.PassesLeft)
	}
}

// The predetermined adjudication order depends only on (char order, seq),
// never on the order or grouping in which inputs arrived at the server.
func TestAdjudicationOrderIndependentOfArrival(t *testing.T) {
	mk := func() []Input {
		return []Input{
			{SessionID: "s1", CharID: "c1", Seq: 1, Cmd: CmdRight},
			{SessionID: "s1", CharID: "c1", Seq: 2, Cmd: CmdClaim},
			{SessionID: "s2", CharID: "c2", Seq: 1, Cmd: CmdClaim},
			{SessionID: "s2", CharID: "c2", Seq: 2, Cmd: CmdLeft, DependsOn: 1},
		}
	}
	inOrder := mk()
	// Same inputs, shuffled arrival (as out-of-order network delivery and
	// re-batching would produce).
	shuffled := []Input{inOrder[2], inOrder[3], inOrder[1], inOrder[0]}

	live := resourceRoom()
	appliedLive, _ := live.ApplyTick(inOrder)
	rerun := resourceRoom()
	appliedRerun, _ := rerun.ApplyTick(shuffled)

	if len(appliedLive) != len(appliedRerun) {
		t.Fatalf("applied count differs: %d vs %d", len(appliedLive), len(appliedRerun))
	}
	for i := range appliedLive {
		if appliedLive[i] != appliedRerun[i] {
			t.Fatalf("adjudication %d differs by arrival order:\n%v\n%v", i, appliedLive[i], appliedRerun[i])
		}
	}
	if live.Snapshot().HashValue() != rerun.Snapshot().HashValue() {
		t.Fatal("final state hash differs by arrival order")
	}
	// And the winner is c1 in both runs: the slot went to the
	// predetermined adjudication order, not to whichever prediction
	// happened to arrive last.
	if live.Chars[0].Passes != 1 || live.Chars[1].Passes != 0 {
		t.Fatalf("holder wrong: c1=%d c2=%d", live.Chars[0].Passes, live.Chars[1].Passes)
	}
}

// Resource grants and dependency outcomes survive a full journal replay:
// the rebuilt room reproduces every adjudication record and the final
// resource state bit-for-bit.
func TestResourceStateSurvivesReplay(t *testing.T) {
	r := resourceRoom()
	batches := [][]Input{
		{
			{SessionID: "s1", CharID: "c1", Seq: 1, Cmd: CmdClaim},
			{SessionID: "s2", CharID: "c2", Seq: 1, Cmd: CmdClaim},
		},
		{
			{SessionID: "s2", CharID: "c2", Seq: 2, Cmd: CmdUp, DependsOn: 1},
			{SessionID: "s1", CharID: "c1", Seq: 2, Cmd: CmdRight},
		},
	}
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
	replayed, snaps, err := ReplayRoom("r", Config{Width: 6, Height: 4, Passes: 1, Obstacles: [][2]int{{2, 0}}}, chars2, records)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if snaps[len(snaps)-1].Hash != r.Snapshot().HashValue() {
		t.Fatal("replay hash mismatch on resource state")
	}
	if replayed.PassesLeft != 0 || replayed.Chars[0].Passes != 1 || replayed.Chars[1].Passes != 0 {
		t.Fatalf("replayed resource state wrong: left=%d c1=%d c2=%d",
			replayed.PassesLeft, replayed.Chars[0].Passes, replayed.Chars[1].Passes)
	}
	// Dependency outcome is also rebuilt: a dependent input after replay
	// sees the same rejection as the live room would.
	applied, _ := replayed.ApplyTick([]Input{
		{SessionID: "s2", CharID: "c2", Seq: 3, Cmd: CmdDown, DependsOn: 2},
	})
	if applied[0].Accepted || applied[0].Reason != "dependency-rejected" {
		t.Fatalf("dependency on replayed rejection must fail: %+v", applied[0])
	}
}
