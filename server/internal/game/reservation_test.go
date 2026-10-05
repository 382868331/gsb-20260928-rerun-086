package game

import "testing"

func resvTestRoom() *Room {
	cfg := DefaultConfig()
	chars := []*Char{
		{ID: "c1", Name: "A", OwnerSession: "s1", X: 1, Y: 1},
		{ID: "c2", Name: "B", OwnerSession: "s2", X: 3, Y: 3},
	}
	return NewRoom("r1", cfg, chars)
}

func resvTick(r *Room) Snapshot {
	_, snap := r.ApplyTickCtx(nil, nil)
	r.DrainResvEvents()
	return snap
}

func resvState(r *Room, id string) *ReservationView {
	return r.GetReservation(id)
}

// A group request is approved only when every region has capacity and no
// character overlaps; otherwise the whole request waits — never half
// approved.
func TestReservationAtomicAllOrNothing(t *testing.T) {
	r := resvTestRoom()
	r.SubmitReservation(ReservationRequest{ID: "a", Chars: []string{"c1"}, Regions: []string{"hall", "garden"}, Start: 2, End: 10, Priority: 5})
	r.SubmitReservation(ReservationRequest{ID: "b", Chars: []string{"c2"}, Regions: []string{"garden"}, Start: 2, End: 10, Priority: 1})
	resvTick(r)
	if got := resvState(r, "a").State; got != "approved" {
		t.Fatalf("a state = %s, want approved", got)
	}
	b := resvState(r, "b")
	if b.State != "pending" {
		t.Fatalf("b state = %s, want pending (no partial approval)", b.State)
	}
	if len(b.Conflicts) != 1 || b.Conflicts[0].Kind != "region" || b.Conflicts[0].ID != "garden" || b.Conflicts[0].By != "a" {
		t.Fatalf("b conflicts = %+v, want one garden conflict by a", b.Conflicts)
	}
	if b.Conflicts[0].Start != 2 || b.Conflicts[0].End != 10 {
		t.Fatalf("conflict interval = [%d,%d), want [2,10)", b.Conflicts[0].Start, b.Conflicts[0].End)
	}
	// hall is NOT partially occupied by b: only a occupies anything.
	snap := resvTick(r) // tick 2: a becomes active
	var garden, hall *RegionView
	for i := range snap.Regions {
		switch snap.Regions[i].ID {
		case "garden":
			garden = &snap.Regions[i]
		case "hall":
			hall = &snap.Regions[i]
		}
	}
	if garden == nil || garden.Used != 1 || garden.Occupants[0] != "a" {
		t.Fatalf("garden occupancy = %+v, want used=1 by a", garden)
	}
	if hall == nil || hall.Used != 1 {
		t.Fatalf("hall occupancy = %+v, want used=1", hall)
	}
	// A third request overlapping a's character on a region with spare
	// capacity still waits: character time must not overlap.
	r.SubmitReservation(ReservationRequest{ID: "c", Chars: []string{"c1"}, Regions: []string{"hall"}, Start: 3, End: 9, Priority: 9})
	resvTick(r)
	cv := resvState(r, "c")
	if cv.State != "pending" {
		t.Fatalf("c state = %s, want pending (char overlap)", cv.State)
	}
	foundChar := false
	for _, cf := range cv.Conflicts {
		if cf.Kind == "char" && cf.ID == "c1" && cf.By == "a" {
			foundChar = true
		}
	}
	if !foundChar {
		t.Fatalf("c conflicts = %+v, want char c1 conflict by a", cv.Conflicts)
	}
}

// Competing requests at the same adjudication boundary are decided by
// priority, independent of submission order.
func TestReservationPriorityOrder(t *testing.T) {
	r := resvTestRoom()
	r.SubmitReservation(ReservationRequest{ID: "low", Chars: []string{"c1"}, Regions: []string{"garden"}, Start: 2, End: 8, Priority: 1})
	r.SubmitReservation(ReservationRequest{ID: "high", Chars: []string{"c2"}, Regions: []string{"garden"}, Start: 2, End: 8, Priority: 5})
	resvTick(r)
	if got := resvState(r, "high").State; got != "approved" {
		t.Fatalf("high state = %s, want approved", got)
	}
	if got := resvState(r, "low").State; got != "pending" {
		t.Fatalf("low state = %s, want pending", got)
	}
	// Adjacent windows reuse the same slot: once high's window ends, low
	// (submitted for [2,8)) has expired... submit a fresh adjacent one.
	r.SubmitReservation(ReservationRequest{ID: "adj", Chars: []string{"c1"}, Regions: []string{"garden"}, Start: 8, End: 12, Priority: 0})
	for i := 0; i < 7; i++ {
		resvTick(r)
	}
	if got := resvState(r, "adj").State; got != "approved" && got != "active" {
		t.Fatalf("adjacent-window request state = %s, want approved/active", got)
	}
	if got := resvState(r, "low").State; got != "rejected" {
		t.Fatalf("expired low state = %s, want rejected", got)
	}
	if resvState(r, "low").Reason != "expired" {
		t.Fatalf("low reason = %s, want expired", resvState(r, "low").Reason)
	}
}

// A long-waiting low-priority request gains a bounded aging promotion and
// can beat a slightly higher-priority newcomer; the bonus is capped so
// the promotion is limited.
func TestReservationAgingPromotion(t *testing.T) {
	r := resvTestRoom()
	r.SubmitReservation(ReservationRequest{ID: "blocker", Chars: []string{"c1"}, Regions: []string{"garden"}, Start: 2, End: 8, Priority: 0})
	r.SubmitReservation(ReservationRequest{ID: "old", Chars: []string{"c2"}, Regions: []string{"garden"}, Start: 2, End: 10, Priority: 0})
	resvTick(r) // tick 1: blocker approved, old waits
	for i := 0; i < 4; i++ {
		resvTick(r) // ticks 2..5: old keeps waiting, aging bonus grows
	}
	// Newcomer with priority 3 arrives much later.
	r.SubmitReservation(ReservationRequest{ID: "new", Chars: []string{"c1"}, Regions: []string{"garden"}, Start: 7, End: 12, Priority: 3})
	resvTick(r) // tick 6: new joins pending
	resvTick(r) // tick 7
	resvTick(r) // tick 8: blocker completes; old eff=0+5, new eff=3+2 -> tie, older submit wins
	if got := resvState(r, "old").State; got != "approved" && got != "active" {
		t.Fatalf("old state = %s, want approved via aging promotion", got)
	}
	if got := resvState(r, "new").State; got != "pending" {
		t.Fatalf("new state = %s, want pending", got)
	}
	resvTick(r) // tick 9
	resvTick(r) // tick 10: old completes, new approved
	if got := resvState(r, "new").State; got != "approved" && got != "active" {
		t.Fatalf("new state after old completes = %s, want approved/active", got)
	}
}

// Cancel before start frees capacity; a late cancel is an explicit no-op;
// early return releases the remaining window exactly once.
func TestReservationCancelAndReturn(t *testing.T) {
	r := resvTestRoom()
	r.SubmitReservation(ReservationRequest{ID: "a", Chars: []string{"c1"}, Regions: []string{"garden"}, Start: 4, End: 20, Priority: 5})
	resvTick(r) // tick 1: approved
	if got := resvState(r, "a").State; got != "approved" {
		t.Fatalf("a state = %s, want approved", got)
	}
	r.CancelReservation("a")
	resvTick(r) // tick 2: cancelled before start
	if got := resvState(r, "a").State; got != "cancelled" {
		t.Fatalf("a state = %s, want cancelled", got)
	}
	// Capacity freed: a same-window request is approved immediately.
	r.SubmitReservation(ReservationRequest{ID: "b", Chars: []string{"c2"}, Regions: []string{"garden"}, Start: 4, End: 20, Priority: 0})
	resvTick(r) // tick 3
	if got := resvState(r, "b").State; got != "approved" {
		t.Fatalf("b state = %s, want approved after cancel freed capacity", got)
	}
	resvTick(r) // tick 4: b active
	if got := resvState(r, "b").State; got != "active" {
		t.Fatalf("b state = %s, want active", got)
	}
	// Late cancel must not affect the active reservation.
	r.CancelReservation("b")
	resvTick(r) // tick 5
	if got := resvState(r, "b").State; got != "active" {
		t.Fatalf("b state after late cancel = %s, want active (cancel ignored)", got)
	}
	// Early return releases the remaining window; a follower reuses it.
	r.SubmitReservation(ReservationRequest{ID: "c", Chars: []string{"c1"}, Regions: []string{"garden"}, Start: 6, End: 12, Priority: 0})
	resvTick(r) // tick 6: c pending (b still holds [4,20))
	if got := resvState(r, "c").State; got != "pending" {
		t.Fatalf("c state = %s, want pending", got)
	}
	r.ReturnReservation("b")
	resvTick(r) // tick 7: b released, c approved
	if got := resvState(r, "b").State; got != "released" {
		t.Fatalf("b state = %s, want released", got)
	}
	if got := resvState(r, "b").ReturnedTick; got != 7 {
		t.Fatalf("b returnedTick = %d, want 7", got)
	}
	if got := resvState(r, "c").State; got != "approved" && got != "active" {
		t.Fatalf("c state = %s, want approved after early return", got)
	}
	// Duplicate return is an idempotent no-op.
	r.ReturnReservation("b")
	resvTick(r)
	if got := resvState(r, "b").ReturnedTick; got != 7 {
		t.Fatalf("b returnedTick after duplicate return = %d, want 7", got)
	}
}

// Duplicate submissions keep the original request identity and never
// consume capacity twice.
func TestReservationIdempotentSubmit(t *testing.T) {
	r := resvTestRoom()
	req := ReservationRequest{ID: "dup", Chars: []string{"c1"}, Regions: []string{"garden"}, Start: 2, End: 6, Priority: 1}
	r.SubmitReservation(req)
	r.SubmitReservation(req) // duplicate delivery before the boundary
	resvTick(r)
	r.SubmitReservation(req) // duplicate delivery after approval
	resvTick(r)
	count := 0
	for _, v := range r.Snapshot().Reservations {
		if v.ID == "dup" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("reservation count for dup = %d, want 1", count)
	}
	snap := resvTick(r) // tick 3: active
	for _, rg := range snap.Regions {
		if rg.ID == "garden" && rg.Used != 1 {
			t.Fatalf("garden used = %d, want exactly 1 (no double consumption)", rg.Used)
		}
	}
}

// Journaled intake ops and verdicts replay to identical snapshots.
func TestReservationReplayMatchesLive(t *testing.T) {
	r := resvTestRoom()
	var events []JournalEvent
	var liveHashes []string
	submit := func(req ReservationRequest) {
		ev := ResvEvent{Type: "resv", Op: "submit", Tick: r.Tick + 1, Request: &req}
		events = append(events, JournalEvent{Resv: &ev})
		r.SubmitReservation(req)
	}
	op := func(op, id string) {
		ev := ResvEvent{Type: "resv", Op: op, Tick: r.Tick + 1, ID: id}
		events = append(events, JournalEvent{Resv: &ev})
		switch op {
		case "cancel":
			r.CancelReservation(id)
		case "return":
			r.ReturnReservation(id)
		}
	}
	tick := func() {
		applied, snap := r.ApplyTickCtx(nil, nil)
		rec := TickRecord{Type: "tick", Tick: snap.Tick, Inputs: applied}
		events = append(events, JournalEvent{Tick: &rec})
		for _, rev := range r.DrainResvEvents() {
			rev2 := rev
			events = append(events, JournalEvent{Resv: &rev2})
		}
		snap.Hash = snap.HashValue()
		liveHashes = append(liveHashes, snap.Hash)
	}
	submit(ReservationRequest{ID: "a", Chars: []string{"c1"}, Regions: []string{"hall", "garden"}, Start: 2, End: 8, Priority: 5})
	submit(ReservationRequest{ID: "b", Chars: []string{"c2"}, Regions: []string{"garden", "vault"}, Start: 2, End: 8, Priority: 1})
	tick()
	tick()
	submit(ReservationRequest{ID: "c", Chars: []string{"c2"}, Regions: []string{"vault"}, Start: 3, End: 10, Priority: 3})
	tick()
	op("cancel", "c")
	tick()
	tick()
	op("return", "a")
	tick()
	for i := 0; i < 6; i++ {
		tick()
	}
	initial := []*Char{
		{ID: "c1", Name: "A", OwnerSession: "s1", X: 1, Y: 1},
		{ID: "c2", Name: "B", OwnerSession: "s2", X: 3, Y: 3},
	}
	_, snaps, err := ReplayJournal("r1", DefaultConfig(), initial, events)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(snaps) != len(liveHashes) {
		t.Fatalf("replayed %d snapshots, want %d", len(snaps), len(liveHashes))
	}
	for i, snap := range snaps {
		if snap.HashValue() != liveHashes[i] {
			t.Fatalf("snapshot %d (tick %d) hash diverged", i, snap.Tick)
		}
	}
	// Closed verdicts are preserved: b expired-rejected or approved state
	// is identical after replay.
	final := snaps[len(snaps)-1]
	states := map[string]string{}
	for _, v := range final.Reservations {
		states[v.ID] = v.State
	}
	if states["a"] != "released" || states["c"] != "cancelled" {
		t.Fatalf("final states = %v, want a=released c=cancelled", states)
	}
}
