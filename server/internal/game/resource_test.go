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

// settleRuns advances empty ticks until every open contention round has
// closed, returning the applied records and snapshot of the final tick.
func settleRounds(r *Room) ([]AppliedInput, Snapshot) {
	var applied []AppliedInput
	var snap Snapshot
	for r.HasOpenRound() {
		a, s := r.ApplyTick(nil)
		applied = append(applied, a...)
		snap = s
	}
	return applied, snap
}

// findVerdict locates the adjudication record a round produced for a
// claim (as opposed to the collection record at arrival).
func findVerdict(applied []AppliedInput, charID string, seq int64) AppliedInput {
	for _, a := range applied {
		if a.CharID == charID && a.Seq == seq && a.Derived {
			return a
		}
	}
	return AppliedInput{}
}

// Two clients claim the same single-slot resource: the claims are
// collected into one contention round with a persistent identity, and at
// the collection boundary the winner is fixed by stable order (character
// declaration, then seq); the loser is rejected with an explicit reason.
func TestContendedClaimSingleWinner(t *testing.T) {
	r, a, b := claimRoom()
	inA := Input{SessionID: "sa", CharID: a.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass", RuleVersion: 1}
	inB := Input{SessionID: "sb", CharID: b.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass", RuleVersion: 1}
	applied, _ := r.ApplyTick([]Input{inB, inA}) // arrival order must not matter
	recA := findApplied(applied, a.ID, 1)
	recB := findApplied(applied, b.ID, 1)
	// At arrival the claims are only collected: undecided actions must
	// not surface as successes (or rejections) yet.
	if !recA.Collected || !recB.Collected || recA.Accepted || recB.Accepted {
		t.Fatalf("claims must be collected undecided: %+v %+v", recA, recB)
	}
	if recA.RoundID == "" || recA.RoundID != recB.RoundID {
		t.Fatalf("both claims must share one round identity: %+v %+v", recA, recB)
	}
	rounds := r.OpenRounds()
	if len(rounds) != 1 || rounds[0].State != "collecting" || rounds[0].CloseTick != r.Tick+RoundCollectTicks {
		t.Fatalf("open round = %+v", rounds)
	}
	verdicts, snap := settleRounds(r)
	recA = findVerdict(verdicts, a.ID, 1)
	recB = findVerdict(verdicts, b.ID, 1)
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
	if pass.Holders[0].Round != recA.RoundID {
		t.Fatalf("grant does not name its adjudication round: %+v", pass.Holders[0])
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
	r1.ApplyTick([]Input{
		{SessionID: "sa", CharID: a1.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass"},
		{SessionID: "sb", CharID: b1.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass"},
	})
	_, s1 := settleRounds(r1)
	r2, a2, b2 := claimRoom()
	r2.ApplyTick([]Input{
		{SessionID: "sb", CharID: b2.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass"},
		{SessionID: "sa", CharID: a2.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass"},
	})
	_, s2 := settleRounds(r2)
	if s1.HashValue() != s2.HashValue() {
		t.Fatalf("arrival order changed arbitration: %s vs %s", s1.HashValue(), s2.HashValue())
	}
	if resourceOf(s2, "pass").Holders[0].CharID != a2.ID {
		t.Fatalf("winner changed with arrival order: %+v", resourceOf(s2, "pass"))
	}
}

// The core boundary bug: the SAME two claims, split into different
// arrival batches (different ticks), must still produce the same winner.
// The contention round collects across its explicit boundary and
// adjudicates in stable order, so batching cannot change the outcome.
func TestClaimArbitrationIndependentOfBatching(t *testing.T) {
	run := func(first, second Input) (Snapshot, string) {
		r, _, _ := claimRoom()
		r.ApplyTick([]Input{first})  // batch 1
		r.ApplyTick([]Input{second}) // batch 2: different tick, same round
		verdicts, snap := settleRounds(r)
		// Both claims were adjudicated in one shared round.
		vA := findVerdict(verdicts, "ca", 1)
		vB := findVerdict(verdicts, "cb", 1)
		if vA.RoundID == "" || vA.RoundID != vB.RoundID {
			t.Fatalf("claims split across batches lost their shared round: %+v %+v", vA, vB)
		}
		return snap, resourceOf(snap, "pass").Holders[0].CharID
	}
	inA := Input{SessionID: "sa", CharID: "ca", Seq: 1, Cmd: CmdClaim, Resource: "pass"}
	inB := Input{SessionID: "sb", CharID: "cb", Seq: 1, Cmd: CmdClaim, Resource: "pass"}
	s1, w1 := run(inA, inB) // A's batch arrives first
	s2, w2 := run(inB, inA) // B's batch arrives first
	if w1 != "ca" || w2 != "ca" {
		t.Fatalf("batching changed the winner: %s vs %s", w1, w2)
	}
	if s1.HashValue() != s2.HashValue() {
		t.Fatalf("batching changed the authoritative state: %s vs %s", s1.HashValue(), s2.HashValue())
	}
}

// A dependent action is refused with its own independent reason when its
// prerequisite was rejected; unrelated actions keep processing.
func TestDependencyFailureIndependentReason(t *testing.T) {
	r, a, b := claimRoom()
	// Tick 1: claims collected; the round closes two ticks later and A
	// wins the pass; B's claim is exhausted.
	r.ApplyTick([]Input{
		{SessionID: "sa", CharID: a.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass"},
		{SessionID: "sb", CharID: b.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass"},
	})
	settleRounds(r)
	// After the verdict: B's dependent move fails with its own reason;
	// B's independent move is still processed normally.
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

// A dependent action formed while its prerequisite claim is still being
// adjudicated is neither executed nor rejected early: it is deferred
// into the round and decided with the verdict.
func TestDependentWaitsForAdjudication(t *testing.T) {
	r, a, b := claimRoom()
	r.ApplyTick([]Input{
		{SessionID: "sa", CharID: a.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass"},
		{SessionID: "sb", CharID: b.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass"},
	})
	// Tick 2, round still open: B's dependent move must be deferred, not
	// rejected with a premature dependency-failed.
	applied, _ := r.ApplyTick([]Input{
		{SessionID: "sb", CharID: b.ID, Seq: 2, Cmd: CmdUp, DependsOn: 1},
	})
	rec := findApplied(applied, b.ID, 2)
	if !rec.Deferred || rec.Accepted || rec.Reason != "" || rec.RoundID == "" {
		t.Fatalf("dependent before verdict = %+v, want deferred undecided", rec)
	}
	verdicts, _ := settleRounds(r)
	dep := findVerdict(verdicts, b.ID, 2)
	if dep.Accepted || dep.Reason != "dependency-failed" {
		t.Fatalf("deferred dependent = %+v, want dependency-failed after verdict", dep)
	}
	// And a dependent of the WINNING claim executes once the grant lands.
	r2, a2, _ := claimRoom()
	r2.ApplyTick([]Input{
		{SessionID: "sa", CharID: a2.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass"},
	})
	r2.ApplyTick([]Input{
		{SessionID: "sa", CharID: a2.ID, Seq: 2, Cmd: CmdRight, DependsOn: 1},
	})
	verdicts, _ = settleRounds(r2)
	if dep := findVerdict(verdicts, a2.ID, 2); !dep.Accepted {
		t.Fatalf("dependent of granted claim = %+v, want accepted", dep)
	}
	if a2.X != 3 {
		t.Fatalf("deferred move executed at x=%d, want 3", a2.X)
	}
}

// A duplicated or late-redelivered claim can never grant the resource
// twice, and one character can never hold the same resource twice.
func TestClaimNeverGrantedTwice(t *testing.T) {
	r, a, _ := claimRoom()
	in := Input{SessionID: "sa", CharID: a.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass"}
	r.ApplyTick([]Input{in})
	verdicts, _ := settleRounds(r)
	if !findVerdict(verdicts, a.ID, 1).Accepted {
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
	// A second, distinct claim by the same character is refused too —
	// in its own fresh round, adjudicated at that round's boundary.
	r.ApplyTick([]Input{{SessionID: "sa", CharID: a.ID, Seq: 2, Cmd: CmdClaim, Resource: "pass"}})
	verdicts, _ = settleRounds(r)
	if rec := findVerdict(verdicts, a.ID, 2); rec.Accepted || rec.Reason != "already-claimed" {
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
	r.ApplyTick([]Input{
		{SessionID: "sa", CharID: a.ID, Seq: 1, Cmd: CmdClaim, Resource: "pass", RuleVersion: 1},
	})
	verdicts, _ := settleRounds(r)
	rec := findVerdict(verdicts, a.ID, 1)
	if rec.Accepted || rec.Reason != "sealed" || !rec.NeedsReconfirm {
		t.Fatalf("claim under closed rules = %+v", rec)
	}
	if rec.RuleVersion != 1 || rec.AppliedRuleVersion != 2 {
		t.Fatalf("basis not preserved: %+v", rec)
	}
	// Re-confirmed under the new rules still fails (claims stay closed),
	// but now without needsReconfirm since the basis is current.
	r.ApplyTick([]Input{
		{SessionID: "sa", CharID: a.ID, Seq: 2, Cmd: CmdClaim, Resource: "pass", RuleVersion: 2},
	})
	verdicts, _ = settleRounds(r)
	rec = findVerdict(verdicts, a.ID, 2)
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
	var apply func(inputs ...Input)
	apply = func(inputs ...Input) {
		applied, snap := r.ApplyTick(inputs)
		rec := TickRecord{Type: "tick", Tick: snap.Tick, Inputs: applied}
		records = append(records, rec)
		events = append(events, JournalEvent{Tick: &records[len(records)-1]})
		for _, rev := range r.DrainRoundEvents() {
			rev := rev
			events = append(events, JournalEvent{Round: &rev})
		}
		snap.Hash = snap.HashValue()
		hashes = append(hashes, snap.Hash)
		// Close any open contention rounds with recorded empty ticks so
		// the journal covers the full adjudication.
		for r.HasOpenRound() {
			apply()
		}
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
