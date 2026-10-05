package game

import "sort"

// Multi-region group pass reservations.
//
// A reservation request asks for several characters to jointly occupy one
// slot each in several regions over a tick window [Start, End). Approval is
// atomic: either every region has capacity for the whole party and none of
// the characters overlaps its own existing bookings, or the request stays
// pending (and later expires) — there is no half-approved state.
//
// Competing requests are adjudicated at tick boundaries in a stable,
// replayable order: effective priority (declared priority plus a capped
// aging boost for long waits), then submission tick, then request id. The
// arrival order of network messages never decides the winner. Requests
// that still cannot be satisfied carry the actual conflicting intervals so
// clients can show why they are waiting.
//
// All facts are journaled: requests ("resvreq"), cancel/release ops
// ("resvop") and derived decisions ("resv"). Replay re-derives decisions
// and cross-checks them, so reconnects, duplicate submissions and
// reordered redelivery never change a closed verdict or consume a slot
// twice.

// RegionSpec declares one bookable region of a room.
type RegionSpec struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Capacity int    `json:"capacity"`
}

// Aging: a waiting request gains +1 effective priority every
// ResvAgingEveryTicks ticks, capped at ResvAgingCap — long-waiting
// low-priority requests get a limited promotion chance, never unlimited.
const (
	ResvAgingEveryTicks = 10
	ResvAgingCap        = 3
)

// ResvRequest is the client-submitted, journaled identity of a group
// reservation. ID is client-generated so a retried submission is
// idempotent: the same ID never creates a second request.
type ResvRequest struct {
	Type     string   `json:"type"` // always "resvreq"
	ID       string   `json:"id"`
	CharIDs  []string `json:"charIds"`
	Regions  []string `json:"regions"`
	Start    int64    `json:"start"`
	End      int64    `json:"end"`
	Priority int      `json:"priority"`
}

// ResvOp is a cancel/release operation applied at a tick boundary.
type ResvOp struct {
	Op string `json:"op"` // "cancel" | "release"
	ID string `json:"id"`
}

// ResvOpRecord is the journaled form of a ResvOp; it takes effect at Tick
// (the tick right after it was journaled), like a rule switch.
type ResvOpRecord struct {
	Type string `json:"type"` // always "resvop"
	Tick int64  `json:"tick"`
	Op   string `json:"op"`
	ID   string `json:"id"`
}

// ResvConflict names one concrete interval that blocks a request.
type ResvConflict struct {
	Kind   string `json:"kind"` // "capacity" | "char-overlap"
	Region string `json:"region,omitempty"`
	CharID string `json:"charId,omitempty"`
	From   int64  `json:"from"`
	To     int64  `json:"to"`
	With   string `json:"with"` // conflicting reservation id
}

// Reservation is the authoritative state of one request.
type Reservation struct {
	ID         string
	CharIDs    []string
	Regions    []string
	Start      int64
	End        int64 // truncated by an early release
	Priority   int
	Boost      int // aging boost at last adjudication (display)
	SubmitTick int64
	// Decision: "pending" | "approved" | "cancelled" | "released" | "expired".
	Decision    string
	DecidedTick int64
	Conflicts   []ResvConflict
}

// ResvEvent is the journaled, derived decision of the adjudicator
// ("approved" / "expired"). Replay re-derives it and cross-checks.
type ResvEvent struct {
	Type      string         `json:"type"` // always "resv"
	ID        string         `json:"id"`
	State     string         `json:"state"`
	Tick      int64          `json:"tick"`
	Conflicts []ResvConflict `json:"conflicts,omitempty"`
}

// ResvView is the snapshot view of one reservation. Phase derives the
// user-facing stage from the decision and the current tick.
type ResvView struct {
	ID         string         `json:"id"`
	CharIDs    []string       `json:"charIds"`
	Regions    []string       `json:"regions"`
	Start      int64          `json:"start"`
	End        int64          `json:"end"`
	Priority   int            `json:"priority"`
	Boost      int            `json:"boost"`
	SubmitTick int64          `json:"submitTick"`
	Phase      string         `json:"phase"` // pending|approved|active|completed|released|cancelled|expired
	Conflicts  []ResvConflict `json:"conflicts,omitempty"`
}

func (rv *Reservation) phase(tick int64) string {
	switch rv.Decision {
	case "approved":
		switch {
		case tick >= rv.End:
			return "completed"
		case tick >= rv.Start:
			return "active"
		}
		return "approved"
	default:
		return rv.Decision
	}
}

// effPriority is the declared priority plus the capped aging boost.
func (rv *Reservation) effPriority(tick int64) int {
	waited := tick - rv.SubmitTick
	if waited < 0 {
		waited = 0
	}
	boost := int(waited / ResvAgingEveryTicks)
	if boost > ResvAgingCap {
		boost = ResvAgingCap
	}
	return rv.Priority + boost
}

// overlaps reports whether [aStart,aEnd) and [bStart,bEnd) share any tick.
// Adjacent windows (aEnd == bStart) do not overlap and can reuse a slot.
func overlaps(aStart, aEnd, bStart, bEnd int64) bool {
	return aStart < bEnd && bStart < aEnd
}

func hasStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// occupying returns the reservations currently holding capacity: approved
// or released ones whose (possibly truncated) window overlaps [start,end).
func (r *Room) occupying(start, end int64, extra ...*Reservation) []*Reservation {
	var out []*Reservation
	for _, id := range r.resvOrder {
		rv := r.resv[id]
		if rv.Decision != "approved" && rv.Decision != "released" {
			continue
		}
		if overlaps(rv.Start, rv.End, start, end) {
			out = append(out, rv)
		}
	}
	for _, rv := range extra {
		if overlaps(rv.Start, rv.End, start, end) {
			out = append(out, rv)
		}
	}
	return out
}

// findConflicts checks a request atomically against every region and every
// character. A nil result means the whole party can be approved in all
// regions; otherwise the actual blocking intervals are returned.
func (r *Room) findConflicts(req *Reservation, extra ...*Reservation) []ResvConflict {
	var conflicts []ResvConflict
	occ := r.occupying(req.Start, req.End, extra...)
	for _, region := range req.Regions {
		spec, ok := r.regions[region]
		if !ok {
			continue // validated at intake
		}
		used := 0
		var blockers []*Reservation
		for _, v := range occ {
			if v.ID != req.ID && hasStr(v.Regions, region) {
				used += len(v.CharIDs)
				blockers = append(blockers, v)
			}
		}
		if used+len(req.CharIDs) > spec.Capacity {
			for _, b := range blockers {
				conflicts = append(conflicts, ResvConflict{
					Kind: "capacity", Region: region,
					From: max64(req.Start, b.Start), To: min64(req.End, b.End), With: b.ID,
				})
			}
			if len(blockers) == 0 {
				conflicts = append(conflicts, ResvConflict{
					Kind: "capacity", Region: region, From: req.Start, To: req.End,
				})
			}
		}
	}
	for _, charID := range req.CharIDs {
		for _, v := range occ {
			if v.ID == req.ID || !hasStr(v.CharIDs, charID) {
				continue
			}
			conflicts = append(conflicts, ResvConflict{
				Kind: "char-overlap", CharID: charID,
				From: max64(req.Start, v.Start), To: min64(req.End, v.End), With: v.ID,
			})
		}
	}
	return conflicts
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// addReservation registers a freshly journaled request at the current tick.
func (r *Room) addReservation(req ResvRequest) {
	if _, dup := r.resv[req.ID]; dup {
		return // replay/duplicate safety: identity is never consumed twice
	}
	r.resv[req.ID] = &Reservation{
		ID: req.ID, CharIDs: append([]string(nil), req.CharIDs...),
		Regions: append([]string(nil), req.Regions...),
		Start:   req.Start, End: req.End, Priority: req.Priority,
		SubmitTick: r.Tick, Decision: "pending",
	}
	r.resvOrder = append(r.resvOrder, req.ID)
}

// applyResvOp applies a cancel/release op at the current tick. Late
// cancels (already started) and duplicate releases are explicit no-ops:
// they never touch capacity that later grantees rely on.
func (r *Room) applyResvOp(op ResvOp) {
	rv := r.resv[op.ID]
	if rv == nil {
		return
	}
	switch op.Op {
	case "cancel":
		if rv.Decision == "pending" || (rv.Decision == "approved" && r.Tick < rv.Start) {
			rv.Decision = "cancelled"
			rv.DecidedTick = r.Tick
		}
	case "release":
		if rv.Decision == "approved" && r.Tick >= rv.Start && r.Tick < rv.End {
			rv.End = r.Tick // return the remaining window
			rv.Decision = "released"
			rv.DecidedTick = r.Tick
		}
	}
}

// adjudicateReservations runs once per tick: pending requests whose start
// has passed expire with their final conflicts; the rest are tried in the
// stable adjudication order and approved atomically when every region and
// character constraint holds.
func (r *Room) adjudicateReservations() {
	var pend []*Reservation
	for _, id := range r.resvOrder {
		rv := r.resv[id]
		if rv.Decision == "pending" {
			rv.Boost = rv.effPriority(r.Tick) - rv.Priority
			pend = append(pend, rv)
		}
	}
	sort.SliceStable(pend, func(i, j int) bool {
		pi, pj := pend[i].effPriority(r.Tick), pend[j].effPriority(r.Tick)
		if pi != pj {
			return pi > pj
		}
		if pend[i].SubmitTick != pend[j].SubmitTick {
			return pend[i].SubmitTick < pend[j].SubmitTick
		}
		return pend[i].ID < pend[j].ID
	})
	var approvedThisPass []*Reservation
	for _, rv := range pend {
		conflicts := r.findConflicts(rv, approvedThisPass...)
		if len(conflicts) > 0 {
			rv.Conflicts = conflicts
			continue
		}
		rv.Decision = "approved"
		rv.DecidedTick = r.Tick
		rv.Conflicts = nil
		approvedThisPass = append(approvedThisPass, rv)
		r.resvEvents = append(r.resvEvents, ResvEvent{
			Type: "resv", ID: rv.ID, State: "approved", Tick: r.Tick,
		})
	}
	// A request still blocked when its start tick arrives expires with
	// its final conflicts: a closed verdict, never a silent drop.
	for _, id := range r.resvOrder {
		rv := r.resv[id]
		if rv.Decision == "pending" && r.Tick >= rv.Start {
			rv.Conflicts = r.findConflicts(rv)
			rv.Decision = "expired"
			rv.DecidedTick = r.Tick
			r.resvEvents = append(r.resvEvents, ResvEvent{
				Type: "resv", ID: rv.ID, State: "expired", Tick: r.Tick,
				Conflicts: rv.Conflicts,
			})
		}
	}
}

// GetReservation returns one reservation by id.
func (r *Room) GetReservation(id string) (*Reservation, bool) {
	rv, ok := r.resv[id]
	return rv, ok
}

// ReservationView is the snapshot view of one reservation at the current
// tick.
func (r *Room) ReservationView(id string) (ResvView, bool) {
	rv, ok := r.resv[id]
	if !ok {
		return ResvView{}, false
	}
	return r.resvViewOf(rv), true
}

func (r *Room) resvViewOf(rv *Reservation) ResvView {
	return ResvView{
		ID: rv.ID, CharIDs: append([]string(nil), rv.CharIDs...),
		Regions: append([]string(nil), rv.Regions...),
		Start:   rv.Start, End: rv.End, Priority: rv.Priority, Boost: rv.Boost,
		SubmitTick: rv.SubmitTick, Phase: rv.phase(r.Tick),
		Conflicts: append([]ResvConflict(nil), rv.Conflicts...),
	}
}

// HasPendingReservations reports whether any request still awaits
// adjudication; the tick loop keeps advancing while one does.
func (r *Room) HasPendingReservations() bool {
	for _, id := range r.resvOrder {
		if r.resv[id].Decision == "pending" {
			return true
		}
	}
	return false
}

// DrainResvEvents returns the derived reservation decisions since the last
// call; the intake layer journals them for replay cross-checking.
func (r *Room) DrainResvEvents() []ResvEvent {
	ev := r.resvEvents
	r.resvEvents = nil
	return ev
}
