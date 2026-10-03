package game

import "testing"

func claimRoom() (*Room, *Char, *Char) {
	cfg := DefaultConfig()
	a := &Char{ID: "ca", Name: "A", OwnerSession: "sa", X: 2, Y: 2, Facing: CmdRight}
	b := &Char{ID: "cb", Name: "B", OwnerSession: "sb", X: 15, Y: 7, Facing: CmdLeft}
	return NewRoom("r", cfg, []*Char{a, b}), a, b
}

func findApplied(applied []AppliedInput, charID string, seq int64) AppliedInput {
	for _, a := range applied {
		if a.CharID == charID && a.Seq == seq {
			return a
		}
	}
	return AppliedInput{}
}

func resourceOf(s Snapshot, id string) ResourceState {
	for _, rs := range s.Resources {
		if rs.ID == id {
			return rs
		}
	}
	return ResourceState{}
}

// Two clients claim the same single-slot resource in the same tick: the
// winner is fixed by deterministic order (character declaration, then
// seq), the loser is rejected with an explicit reason.
func TestContendedClaimSingleWinner(t *testing.T) {
	r, a, b := claimRoom()
	inA := Input{SessionID: "sa", CharID: a.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass", RuleVersion: 1}
	inB := Input{SessionID: "sb", CharID: b.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass", RuleVersion: 1}
	applied, snap := r.ApplyTick([]Input{inB, inA}) // arrival order must not matter
	recA := findApplied(applied, a.ID, 1)
	recB := findApplied(applied, b.ID, 1)
	if !recA.Accepted {
		t.Fatalf("char A claim rejected: %+v", recA)
	}
	if recB.Accepted || recB.Reason != "resource-exhausted" {
		t.Fatalf("char B claim = %+v, want resource-exhausted", recB)
	}
	pass := resourceOf(snap, "pass")
	if pass.Remaining != 0 || len(pass.Holders) != 1 || pass.Holders[0].CharID != a.ID {
		t.Fatalf("pass state = %+v", pass)
	}
	// The other resource is untouched.
	if relic := resourceOf(snap, "relic"); relic.Remaining != 1 {
		t.Fatalf("relic remaining = %d, want 1", relic.Remaining)
	}
}

// The same set of inputs, delivered in different orders and batchings that
// still land in one tick, produces the identical arbitration outcome.
func TestClaimArbitrationIndependentOfArrivalOrder(t *testing.T) {
	r1, a1, b1 := claimRoom()
	_, s1 := r1.ApplyTick([]Input{
		{SessionID: "sa", CharID: a1.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass"},
		{SessionID: "sb", CharID: b1.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass"},
	})
	r2, a2, b2 := claimRoom()
	_, s2 := r2.ApplyTick([]Input{
		{SessionID: "sb", CharID: b2.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass"},
		{SessionID: "sa", CharID: a2.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass"},
	})
	if s1.HashValue() != s2.HashValue() {
		t.Fatalf("arrival order changed arbitration: %s vs %s", s1.HashValue(), s2.HashValue())
	}
	if resourceOf(s2, "pass").Holders[0].CharID != a2.ID {
		t.Fatalf("winner changed with arrival order: %+v", resourceOf(s2, "pass"))
	}
}

// A dependent action is refused with its own independent reason when its
// prerequisite was rejected; unrelated actions keep processing.
func TestDependencyFailureIndependentReason(t *testing.T) {
	r, a, b := claimRoom()
	// Tick 1: A wins the pass; B's claim is exhausted.
	r.ApplyTick([]Input{
		{SessionID: "sa", CharID: a.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass"},
		{SessionID: "sb", CharID: b.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass"},
	})
	// Tick 2: B's dependent move fails with its own reason; B's independent
	// move is still processed normally.
	applied, _ := r.ApplyTick([]Input{
		{SessionID: "sb", CharID: b.ID, Seq: 2, Cmd: CmdUp, DependsOn: 1},
		{SessionID: "sb", CharID: b.ID, Seq: 3, Cmd: CmdLeft},
	})
	dep := findApplied(applied, b.ID, 2)
	if dep.Accepted || dep.Reason != "dependency-failed" {
		t.Fatalf("dependent move = %+v, want dependency-failed", dep)
	}
	indep := findApplied(applied, b.ID, 3)
	if !indep.Accepted {
		t.Fatalf("independent move = %+v, want accepted", indep)
	}
	if b.X != 14 || b.Y != 7 {
		t.Fatalf("B at (%d,%d), want (14,7): independent action applied, dependent skipped", b.X, b.Y)
	}
	// A dependency on a succeeded action does not block the follow-up.
	applied, _ = r.ApplyTick([]Input{
		{SessionID: "sa", CharID: a.ID, Seq: 2, Cmd: CmdRight, DependsOn: 1},
	})
	if rec := findApplied(applied, a.ID, 2); !rec.Accepted {
		t.Fatalf("dependent on successful claim = %+v, want accepted", rec)
	}
}

// A duplicated or late-redelivered claim can never grant the resource
// twice, and one character can never hold the same resource twice.
func TestClaimNeverGrantedTwice(t *testing.T) {
	r, a, _ := claimRoom()
	in := Input{SessionID: "sa", CharID: a.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass"}
	applied, _ := r.ApplyTick([]Input{in})
	if !findApplied(applied, a.ID, 1).Accepted {
		t.Fatal("first claim rejected")
	}
	// Exact duplicate in a later tick: stale, not executed again.
	applied, snap := r.ApplyTick([]Input{in})
	if len(applied) != 0 {
		t.Fatalf("duplicate claim executed: %+v", applied)
	}
	if pass := resourceOf(snap, "pass"); pass.Remaining != 0 || len(pass.Holders) != 1 {
		t.Fatalf("duplicate grant: %+v", pass)
	}
	// A second, distinct claim by the same character is refused too.
	applied, _ = r.ApplyTick([]Input{{SessionID: "sa", CharID: a.ID, Seq: 2, Cmd: CmdClaim, Resource: "pass"}})
	if rec := findApplied(applied, a.ID, 2); rec.Accepted || rec.Reason != "already-claimed" {
		t.Fatalf("second claim = %+v, want already-claimed", rec)
	}
}

// A claim formed under old rules but judged after claims were closed is
// rejected with needsReconfirm; the original rule basis is preserved.
func TestClaimRuleSwitchNeedsReconfirm(t *testing.T) {
	r, a, _ := claimRoom()
	rules := r.Rules
	rules.Version++
	rules.ClaimsClosed = true
	r.SetRules(rules)
	applied, _ := r.ApplyTick([]Input{
		{SessionID: "sa", CharID: a.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass", RuleVersion: 1},
	})
	rec := findApplied(applied, a.ID, 1)
	if rec.Accepted || rec.Reason != "sealed" || !rec.NeedsReconfirm {
		t.Fatalf("claim under closed rules = %+v", rec)
	}
	if rec.RuleVersion != 1 || rec.AppliedRuleVersion != 2 {
		t.Fatalf("basis not preserved: %+v", rec)
	}
	// Re-confirmed under the new rules still fails (claims stay closed),
	// but now without needsReconfirm since the basis is current.
	applied, _ = r.ApplyTick([]Input{
		{SessionID: "sa", CharID: a.ID, Seq: 2, Cmd: CmdClaim, Resource: "pass", RuleVersion: 2},
	})
	rec = findApplied(applied, a.ID, 2)
	if rec.Accepted || rec.Reason != "sealed" || rec.NeedsReconfirm {
		t.Fatalf("re-confirmed claim = %+v", rec)
	}
}

// Claims, dependencies and rule switches replay to identical hashes.
func TestReplayWithClaimsDeterministic(t *testing.T) {
	r, a, b := claimRoom()
	var records []TickRecord
	var events []JournalEvent
	var hashes []string
	apply := func(inputs ...Input) {
		applied, snap := r.ApplyTick(inputs)
		rec := TickRecord{Type: "tick", Tick: snap.Tick, Inputs: applied}
		records = append(records, rec)
		events = append(events, JournalEvent{Tick: &records[len(records)-1]})
		snap.Hash = snap.HashValue()
		hashes = append(hashes, snap.Hash)
	}
	apply(
		Input{SessionID: "sa", CharID: a.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass", RuleVersion: 1},
		Input{SessionID: "sb", CharID: b.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass", RuleVersion: 1},
	)
	rules := r.Rules
	rules.Version++
	rules.ClaimsClosed = true
	r.SetRules(rules)
	rr := RulesRecord{Type: "rules", Tick: r.Tick + 1, Rules: rules}
	events = append(events, JournalEvent{Rules: &rr})
	apply(
		Input{SessionID: "sb", CharID: b.ID, Seq: 2, Cmd: CmdUp, DependsOn: 1, RuleVersion: 1},
		Input{SessionID: "sb", CharID: b.ID, Seq: 3, Cmd: CmdLeft, RuleVersion: 1},
		Input{SessionID: "sa", CharID: a.ID, Seq: 2, Cmd: CmdClaim, Resource: "relic", RuleVersion: 1},
	)
	_, snaps, err := ReplayJournal("r", DefaultConfig(), []*Char{
		{ID: "ca", Name: "A", OwnerSession: "sa", X: 2, Y: 2, Facing: CmdRight},
		{ID: "cb", Name: "B", OwnerSession: "sb", X: 15, Y: 7, Facing: CmdLeft},
	}, events)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != len(hashes) {
		t.Fatalf("replayed %d snapshots, want %d", len(snaps), len(hashes))
	}
	for i, snap := range snaps {
		if snap.Hash != hashes[i] {
			t.Fatalf("tick %d hash mismatch: %s vs %s", snap.Tick, snap.Hash, hashes[i])
		}
	}
	final := snaps[len(snaps)-1]
	if pass := resourceOf(final, "pass"); len(pass.Holders) != 1 || pass.Holders[0].CharID != "ca" {
		t.Fatalf("replayed pass = %+v", pass)
	}
	if relic := resourceOf(final, "relic"); relic.Remaining != 1 {
		t.Fatalf("replayed relic = %+v (claim under closed rules must stay rejected)", relic)
	}
}
