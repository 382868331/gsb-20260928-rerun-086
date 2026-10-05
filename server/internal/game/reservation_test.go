package game

import "testing"

func resvTestRoom() *Room {
	cfg := DefaultConfig()
	chars := []*Char{
		{ID: "c1", Name: "A", OwnerSession: "s1", X: 1, Y: 1},
		{ID: "c2", Name: "B", OwnerSession: "s2", X: 2, Y: 2},
	}
	return NewRoom("r1", cfg, chars)
}

func tickResv(r *Room, reqs []ResvRequest, ops []ResvOp) {
	r.ApplyTickExt(nil, nil, reqs, ops)
}

func req(id string, chars, regions []string, start, end int64, prio int) ResvRequest {
	return ResvRequest{Type: "resvreq", ID: id, CharIDs: chars, Regions: regions, Start: start, End: end, Priority: prio}
}

func decision(t *testing.T, r *Room, id string) string {
	t.Helper()
	rv, ok := r.GetReservation(id)
	if !ok {
		t.Fatalf("reservation %s missing", id)
	}
	return rv.Decision
}

// Atomicity: a party of two asking for hall (cap 2) and garden (cap 1)
// can never be half-approved — garden cannot fit the party, so no region
// is occupied at all.
func TestReservationAtomicAllOrNothing(t *testing.T) {
	r := resvTestRoom()
	tickResv(r, []ResvRequest{req("g1", []string{"c1", "c2"}, []string{"hall", "garden"}, 5, 20, 1)}, nil)
	if d := decision(t, r, "g1"); d != "pending" {
		t.Fatalf("want pending, got %s", d)
	}
	rv, _ := r.GetReservation("g1")
	if len(rv.Conflicts) == 0 {
		t.Fatal("expected concrete conflicts for the unsatisfiable request")
	}
	found := false
	for _, c := range rv.Conflicts {
		if c.Kind == "capacity" && c.Region == "garden" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a garden capacity conflict, got %+v", rv.Conflicts)
	}
	// A hall-only request for the same party must succeed: nothing was
	// occupied by the failed group request.
	tickResv(r, []ResvRequest{req("h1", []string{"c1", "c2"}, []string{"hall"}, 5, 20, 0)}, nil)
	if d := decision(t, r, "h1"); d != "approved" {
		t.Fatalf("hall-only request should be approved, got %s", d)
	}
}

// Cross-region competition at a freeing boundary: when the occupier is
// released early, the waiting requests compete in one adjudication
// pass and the higher priority wins regardless of submission order; the
// loser waits with a concrete conflict interval.
func TestReservationPriorityAtBoundary(t *testing.T) {
	r := resvTestRoom()
	tickResv(r, []ResvRequest{req("occ", []string{"c1"}, []string{"garden"}, 2, 50, 0)}, nil)
	if d := decision(t, r, "occ"); d != "approved" {
		t.Fatalf("occupier not approved: %s", d)
	}
	// Low priority arrives first, high priority later; both are blocked
	// by the occupier and wait.
	tickResv(r, []ResvRequest{req("low", []string{"c2"}, []string{"garden"}, 30, 40, 1)}, nil)
	for i := 0; i < 3; i++ {
		tickResv(r, nil, nil)
	}
	tickResv(r, []ResvRequest{req("high", []string{"c2"}, []string{"garden"}, 30, 40, 5)}, nil)
	// The occupier returns the remaining window early: one adjudication pass decides.
	tickResv(r, nil, []ResvOp{{Op: "release", ID: "occ"}})
	if d := decision(t, r, "high"); d != "approved" {
		t.Fatalf("high priority should win at the boundary, got %s", d)
	}
	if d := decision(t, r, "low"); d != "pending" {
		t.Fatalf("low priority should keep waiting, got %s", d)
	}
	rv, _ := r.GetReservation("low")
	if len(rv.Conflicts) == 0 || rv.Conflicts[0].With != "high" {
		t.Fatalf("low should name the winning reservation as its conflict, got %+v", rv.Conflicts)
	}
}

// Aging: a long-waiting low-priority request earns a capped boost, so
// when the slot frees it can tie a freshly arrived higher-priority one
// and win the wait order — but the boost is limited.
func TestReservationAgingPromotion(t *testing.T) {
	r := resvTestRoom()
	tickResv(r, []ResvRequest{req("occ", []string{"c1"}, []string{"garden"}, 2, 100, 0)}, nil)
	tickResv(r, []ResvRequest{req("old", []string{"c2"}, []string{"garden"}, 60, 70, 2)}, nil)
	// Wait 25 ticks: boost = 25/10 = 2, effective 4.
	for r.Tick < 27 {
		tickResv(r, nil, nil)
	}
	tickResv(r, []ResvRequest{req("new", []string{"c2"}, []string{"garden"}, 60, 70, 4)}, nil)
	// The occupier returns the remaining window: old (eff 2+2=4) ties new (eff 4) and
	// wins the wait order by earlier submission.
	tickResv(r, nil, []ResvOp{{Op: "release", ID: "occ"}})
	if d := decision(t, r, "old"); d != "approved" {
		t.Fatalf("aged request should be promoted, got %s", d)
	}
	if d := decision(t, r, "new"); d != "pending" {
		t.Fatalf("newcomer should keep waiting, got %s", d)
	}
	rv, _ := r.GetReservation("new")
	if len(rv.Conflicts) == 0 || rv.Conflicts[0].With != "old" {
		t.Fatalf("newcomer should name the aged winner, got %+v", rv.Conflicts)
	}
	// The boost is capped: even after a very long wait it never exceeds
	// ResvAgingCap, so promotion stays a limited chance.
	for r.Tick < 90 {
		tickResv(r, nil, nil)
	}
	rv, _ = r.GetReservation("new")
	if rv.Boost != ResvAgingCap {
		t.Fatalf("boost must cap at %d, got %d", ResvAgingCap, rv.Boost)
	}
}

// Adjacent windows [a,b) and [b,c) do not overlap: the same region and
// character can be reused back to back.
func TestReservationAdjacentReuse(t *testing.T) {
	r := resvTestRoom()
	tickResv(r, []ResvRequest{
		req("a", []string{"c1"}, []string{"garden"}, 5, 20, 0),
		req("b", []string{"c1"}, []string{"garden"}, 20, 35, 0),
	}, nil)
	if d := decision(t, r, "a"); d != "approved" {
		t.Fatalf("a: %s", d)
	}
	if d := decision(t, r, "b"); d != "approved" {
		t.Fatalf("adjacent b should reuse the slot, got %s", d)
	}
}

// Cancel before start frees capacity; a late cancel is a no-op; an early
// release truncates the window; a duplicate release changes nothing.
func TestReservationCancelAndRelease(t *testing.T) {
	r := resvTestRoom()
	tickResv(r, []ResvRequest{req("x", []string{"c1"}, []string{"garden"}, 10, 50, 0)}, nil)
	// Cancel before start: frees the slot for the overlapping request.
	tickResv(r, nil, []ResvOp{{Op: "cancel", ID: "x"}})
	if d := decision(t, r, "x"); d != "cancelled" {
		t.Fatalf("x should be cancelled, got %s", d)
	}
	tickResv(r, []ResvRequest{req("y", []string{"c1"}, []string{"garden"}, 10, 50, 0)}, nil)
	if d := decision(t, r, "y"); d != "approved" {
		t.Fatalf("y should take the freed slot, got %s", d)
	}
	// Late cancel (window already started): explicit no-op.
	for r.Tick < 12 {
		tickResv(r, nil, nil)
	}
	tickResv(r, nil, []ResvOp{{Op: "cancel", ID: "y"}})
	if d := decision(t, r, "y"); d != "approved" {
		t.Fatalf("late cancel must not affect a started reservation, got %s", d)
	}
	// Early release returns the remaining window.
	tickResv(r, nil, []ResvOp{{Op: "release", ID: "y"}})
	rv, _ := r.GetReservation("y")
	if rv.Decision != "released" || rv.End != 14 {
		t.Fatalf("release should truncate at the current tick, got %+v", rv)
	}
	end := rv.End
	// Duplicate release: idempotent no-op.
	tickResv(r, nil, []ResvOp{{Op: "release", ID: "y"}})
	rv, _ = r.GetReservation("y")
	if rv.End != end {
		t.Fatalf("duplicate release must not move the end, got %d want %d", rv.End, end)
	}
	// A follow-up request overlapping only the returned tail is approved.
	tickResv(r, []ResvRequest{req("z", []string{"c1"}, []string{"garden"}, 20, 50, 0)}, nil)
	if d := decision(t, r, "z"); d != "approved" {
		t.Fatalf("z should reuse the released tail, got %s", d)
	}
}

// A request that is still blocked when its start arrives expires with its
// final conflict intervals — a closed verdict, never a silent drop.
func TestReservationExpiry(t *testing.T) {
	r := resvTestRoom()
	tickResv(r, []ResvRequest{req("occ", []string{"c1"}, []string{"garden"}, 2, 40, 0)}, nil)
	tickResv(r, []ResvRequest{req("w", []string{"c2"}, []string{"garden"}, 5, 45, 0)}, nil)
	for r.Tick < 5 {
		tickResv(r, nil, nil)
	}
	if d := decision(t, r, "w"); d != "expired" {
		t.Fatalf("w should expire at its start, got %s", d)
	}
	rv, _ := r.GetReservation("w")
	if len(rv.Conflicts) == 0 || rv.Conflicts[0].With != "occ" {
		t.Fatalf("expired request keeps its conflict intervals, got %+v", rv.Conflicts)
	}
}

// Character overlap: the same character cannot hold two overlapping
// bookings even in different regions; a partially offline party (only
// some characters) is still validated per character.
func TestReservationCharOverlap(t *testing.T) {
	r := resvTestRoom()
	tickResv(r, []ResvRequest{req("a", []string{"c1"}, []string{"hall"}, 5, 30, 0)}, nil)
	tickResv(r, []ResvRequest{req("b", []string{"c1", "c2"}, []string{"garden"}, 5, 30, 0)}, nil)
	if d := decision(t, r, "b"); d != "pending" {
		t.Fatalf("b must wait: c1 is already booked, got %s", d)
	}
	rv, _ := r.GetReservation("b")
	found := false
	for _, c := range rv.Conflicts {
		if c.Kind == "char-overlap" && c.CharID == "c1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a char-overlap conflict for c1, got %+v", rv.Conflicts)
	}
}

// Duplicate request ids never create a second request or consume capacity
// twice, even across replay.
func TestReservationDuplicateID(t *testing.T) {
	r := resvTestRoom()
	q := req("dup", []string{"c1"}, []string{"garden"}, 5, 20, 0)
	tickResv(r, []ResvRequest{q}, nil)
	tickResv(r, []ResvRequest{q}, nil)
	if len(r.resvOrder) != 1 {
		t.Fatalf("duplicate id must not create a second request, got %d", len(r.resvOrder))
	}
	if d := decision(t, r, "dup"); d != "approved" {
		t.Fatalf("dup: %s", d)
	}
}

// Replay: requests, ops and derived decisions are re-derived from the
// journal and cross-checked; different arrival batching of the same
// journal yields the same final state.
func TestReservationReplay(t *testing.T) {
	cfg := DefaultConfig()
	chars := []*Char{
		{ID: "c1", Name: "A", OwnerSession: "s1", X: 1, Y: 1},
		{ID: "c2", Name: "B", OwnerSession: "s2", X: 2, Y: 2},
	}
	r1 := NewRoom("r1", cfg, cloneChars(chars))
	var events []JournalEvent
	feed := func(reqs []ResvRequest, ops []ResvOp, n int) {
		for _, q := range reqs {
			q2 := q
			events = append(events, JournalEvent{ResvReq: &q2})
		}
		for i := 0; i < n; i++ {
			tickNo := r1.Tick + 1
			var opRecs []ResvOp
			if i == 0 {
				opRecs = ops
			}
			for _, op := range opRecs {
				rec := ResvOpRecord{Type: "resvop", Tick: tickNo, Op: op.Op, ID: op.ID}
				events = append(events, JournalEvent{ResvOp: &rec})
			}
			_, snap := r1.ApplyTickExt(nil, nil, reqsIf(i, reqs), opRecs)
			snap.Hash = snap.HashValue()
			rec := TickRecord{Type: "tick", Tick: snap.Tick, Inputs: nil}
			events = append(events, JournalEvent{Tick: &rec})
			for _, ev := range r1.DrainResvEvents() {
				ev2 := ev
				events = append(events, JournalEvent{Resv: &ev2})
			}
		}
	}
	feed([]ResvRequest{req("one", []string{"c1", "c2"}, []string{"hall", "vault"}, 10, 40, 3)}, nil, 2)
	feed([]ResvRequest{req("two", []string{"c2"}, []string{"vault"}, 10, 40, 1)}, nil, 6)
	feed(nil, []ResvOp{{Op: "cancel", ID: "one"}}, 2)
	feed(nil, nil, 10)
	r2, snaps, err := ReplayJournal("r1", cfg, chars, events)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(snaps) == 0 {
		t.Fatal("no snapshots replayed")
	}
	if r1.Tick != r2.Tick {
		t.Fatalf("tick divergence %d vs %d", r1.Tick, r2.Tick)
	}
	for _, id := range []string{"one", "two"} {
		v1, _ := r1.ReservationView(id)
		v2, _ := r2.ReservationView(id)
		if v1.Phase != v2.Phase || v1.End != v2.End {
			t.Fatalf("reservation %s diverged: %+v vs %+v", id, v1, v2)
		}
	}
	if d := decision(t, r2, "one"); d != "cancelled" {
		t.Fatalf("one should be cancelled after replay, got %s", d)
	}
	if d := decision(t, r2, "two"); d != "approved" {
		t.Fatalf("two should be approved after the cancel freed vault, got %s", d)
	}
}

func reqsIf(i int, reqs []ResvRequest) []ResvRequest {
	if i == 0 {
		return reqs
	}
	return nil
}
