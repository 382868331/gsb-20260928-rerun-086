package game

import "testing"

// An input formed under older rules that the new rules forbid must be
// rejected with an explicit reason and a re-confirm flag; the original
// rule basis is preserved on the record, never rewritten.
func TestRuleSwitchRejectsStaleBasisWithReconfirm(t *testing.T) {
	r := testRoom()
	// v1: move right twice (1,1)->(2,1)->(3,1)
	r.ApplyTick([]Input{{SessionID: "s1", CharID: "c1", Seq: 1, Cmd: CmdRight, RuleVersion: 1}})
	// Switch to v2: seal (1,1) — the cell the next input targets.
	rules := DefaultRules()
	rules.Version = 2
	rules.Sealed = [][2]int{{1, 1}}
	r.SetRules(rules)

	applied, _ := r.ApplyTick([]Input{
		{SessionID: "s1", CharID: "c1", Seq: 2, Cmd: CmdLeft, RuleVersion: 1},  // formed under v1, now sealed
		{SessionID: "s1", CharID: "c1", Seq: 3, Cmd: CmdRight, RuleVersion: 1}, // depends on seq 2's outcome
	})
	if len(applied) != 2 {
		t.Fatalf("both inputs must be processed in order, got %+v", applied)
	}
	if applied[0].Accepted || applied[0].Reason != "sealed" || !applied[0].NeedsReconfirm {
		t.Fatalf("seq 2 should be sealed+reconfirm, got %+v", applied[0])
	}
	if applied[0].RuleVersion != 1 || applied[0].AppliedRuleVersion != 2 {
		t.Fatalf("rule basis must be preserved: %+v", applied[0])
	}
	// The dependent input is still judged on its own merits: c2 occupies
	// (3,1), so it is blocked for its own reason, unrelated to the switch.
	if applied[1].Accepted || applied[1].Reason != "blocked" || applied[1].NeedsReconfirm {
		t.Fatalf("seq 3 judged independently under v2: %+v", applied[1])
	}
	if r.Chars[0].X != 2 || r.Chars[0].Y != 1 {
		t.Fatalf("pos = (%d,%d), want (2,1)", r.Chars[0].X, r.Chars[0].Y)
	}
}

// A cost change makes an action unaffordable: rejection cites the new
// rules; an input already declared under the new version is rejected
// without the re-confirm flag (the caller knew the rules).
func TestCostChangeRejectsWhenEnergyInsufficient(t *testing.T) {
	r := testRoom()
	r.Chars[0].Energy = 2 // +2 regen at tick = 4, below the new cost of 5
	rules := DefaultRules()
	rules.Version = 2
	rules.Costs[CmdRight] = 5
	r.SetRules(rules)

	applied, _ := r.ApplyTick([]Input{
		{SessionID: "s1", CharID: "c1", Seq: 1, Cmd: CmdRight, RuleVersion: 1},
		{SessionID: "s1", CharID: "c1", Seq: 2, Cmd: CmdRight, RuleVersion: 2},
	})
	if applied[0].Accepted || applied[0].Reason != "insufficient-energy" || !applied[0].NeedsReconfirm {
		t.Fatalf("old-basis input: %+v", applied[0])
	}
	if applied[1].Accepted || applied[1].Reason != "insufficient-energy" || applied[1].NeedsReconfirm {
		t.Fatalf("new-basis input: %+v", applied[1])
	}
}

// Rule switches are journaled events; replaying them interleaved with
// ticks must reproduce the live run exactly.
func TestReplayWithRuleSwitchDeterministic(t *testing.T) {
	mk := func() *Room { return testRoom() }
	live := mk()
	var events []JournalEvent
	run := func(inputs ...Input) {
		applied, snap := live.ApplyTick(inputs)
		snap.Hash = snap.HashValue()
		rec := TickRecord{Type: "tick", Tick: snap.Tick, Inputs: applied}
		events = append(events, JournalEvent{Tick: &rec})
	}
	run(Input{SessionID: "s1", CharID: "c1", Seq: 1, Cmd: CmdRight, RuleVersion: 1})
	rules := DefaultRules()
	rules.Version = 2
	rules.Sealed = [][2]int{{1, 1}}
	live.SetRules(rules)
	rr := RulesRecord{Type: "rules", Tick: live.Tick + 1, Rules: rules}
	events = append(events, JournalEvent{Rules: &rr})
	run(
		Input{SessionID: "s1", CharID: "c1", Seq: 2, Cmd: CmdLeft, RuleVersion: 1},
		Input{SessionID: "s1", CharID: "c1", Seq: 3, Cmd: CmdRight, RuleVersion: 1},
	)

	chars2 := []*Char{
		{ID: "c1", Name: "A", OwnerSession: "s1", X: 1, Y: 1, Facing: CmdRight},
		{ID: "c2", Name: "B", OwnerSession: "s2", X: 3, Y: 1, Facing: CmdLeft},
	}
	replayed, snaps, err := ReplayJournal("r", Config{Width: 6, Height: 4, Obstacles: [][2]int{{2, 0}}}, chars2, events)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("replayed %d snaps, want 2", len(snaps))
	}
	if snaps[len(snaps)-1].Hash != live.Snapshot().HashValue() {
		t.Fatal("replay hash mismatch after rule switch")
	}
	if replayed.Rules.Version != 2 || replayed.Chars[0].X != live.Chars[0].X {
		t.Fatalf("replayed final state diverged: %+v", replayed.Chars[0])
	}
}
