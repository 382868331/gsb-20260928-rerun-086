package game

import "sort"

// Multi-region group reservations ("通行预约").
//
// A reservation asks for several characters to jointly occupy one slot in
// each of several regions during [Start, End). Approval is atomic: either
// every requested region has spare capacity for the whole window and none
// of the characters overlaps another granted reservation, or the request
// stays pending as a whole — there is no state where half the characters
// or half the regions are granted.
//
// Adjudication happens at explicit, replayable boundaries: every tick is
// an adjudication boundary. Pending requests competing at the same
// boundary are ordered by effective priority (declared priority plus a
// bounded aging bonus for long waits), then by submit tick, then by
// request id — never by network arrival order. A request that keeps
// waiting gains +1 effective priority per waited boundary, capped at
// ResvAgingCap, so a long-waiting low-priority request gets a limited
// promotion without ever unseating a clearly newer high-priority one.
//
// Lifecycle: pending -> approved -> active -> completed | released.
// A request can be cancelled before it starts (pending or approved);
// once active, capacity is only released by returning the remaining time
// early (ReturnedTick). A late cancel or a duplicate return is an
// explicit no-op verdict and never touches later grantees. All intake
// operations and all verdicts are journaled so replay re-derives the
// identical sequence of states.
const ResvAgingCap = 5

// RegionSpec declares one bookable region of a room.
type RegionSpec struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Capacity int    `json:"capacity"`
}

// ReservationRequest is the client-supplied, idempotent identity of a
// group reservation. ID is chosen by the client: reconnects, duplicate
// submissions and replays all refer to the same request and never
// consume capacity twice.
type ReservationRequest struct {
	ID       string   `json:"id"`
	Chars    []string `json:"chars"`
	Regions  []string `json:"regions"`
	Start    int64    `json:"start"`
	End      int64    `json:"end"`
	Priority int      `json:"priority"`
}

// ConflictInterval names one concrete overlapping interval that blocks a
// pending request, so an unsatisfiable request shows the actual conflict
// instead of a bare "no capacity".
type ConflictInterval struct {
	Kind  string `json:"kind"` // "region" | "char"
	ID    string `json:"id"`   // region id or character id
	Start int64  `json:"start"`
	End   int64  `json:"end"`
	By    string `json:"by"` // blocking reservation id
}

// Reservation is the authoritative state of one group request.
type Reservation struct {
	ReservationRequest
	SubmitTick int64
	State      string // pending|approved|active|completed|released|cancelled|rejected
	Reason     string
	Conflicts  []ConflictInterval
	// WindowsWaited counts adjudication boundaries at which this request
	// stayed pending; it drives the bounded aging promotion.
	WindowsWaited int
	ReturnedTick  int64 // early return of the remaining window (0 = none)
}

// effectiveEnd is when the reservation stops occupying capacity.
func (res *Reservation) effectiveEnd() int64 {
	if res.ReturnedTick > 0 {
		return res.ReturnedTick
	}
	return res.End
}

// granted reports whether the reservation currently occupies capacity.
func (res *Reservation) granted() bool {
	return res.State == "approved" || res.State == "active"
}

// ResvEvent is the journaled identity of reservation activity. Intake
// operations (submit/cancel/return) are recorded with the tick at which
// they take effect; verdicts are derived by the engine at tick
// boundaries and cross-checked during replay, exactly like round events.
type ResvEvent struct {
	Type      string              `json:"type"` // always "resv"
	Op        string              `json:"op"`   // submit | cancel | return | verdict
	Tick      int64               `json:"tick"`
	Request   *ReservationRequest `json:"request,omitempty"`
	ID        string              `json:"id,omitempty"`
	State     string              `json:"state,omitempty"`
	Reason    string              `json:"reason,omitempty"`
	Conflicts []ConflictInterval  `json:"conflicts,omitempty"`
}

// ReservationView is the snapshot view of one request (part of the hash).
type ReservationView struct {
	ID            string             `json:"id"`
	Chars         []string           `json:"chars"`
	Regions       []string           `json:"regions"`
	Start         int64              `json:"start"`
	End           int64              `json:"end"`
	Priority      int                `json:"priority"`
	SubmitTick    int64              `json:"submitTick"`
	State         string             `json:"state"`
	Reason        string             `json:"reason,omitempty"`
	Conflicts     []ConflictInterval `json:"conflicts,omitempty"`
	WindowsWaited int                `json:"windowsWaited,omitempty"`
	ReturnedTick  int64              `json:"returnedTick,omitempty"`
}

// RegionView is the snapshot view of one region's current occupancy.
type RegionView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Capacity  int      `json:"capacity"`
	Used      int      `json:"used"`
	Occupants []string `json:"occupants"`
}

// SubmitReservation schedules an idempotent submit for the next tick.
// A request id that is already known is ignored: duplicates never
// consume capacity a second time.
func (r *Room) SubmitReservation(req ReservationRequest) {
	r.resvInbox = append(r.resvInbox, ResvEvent{Type: "resv", Op: "submit", Tick: r.Tick + 1, Request: &req})
}

// CancelReservation schedules a cancel for the next tick. It only has an
// effect if the reservation has not started yet; a late cancel is an
// explicit no-op verdict.
func (r *Room) CancelReservation(id string) {
	r.resvInbox = append(r.resvInbox, ResvEvent{Type: "resv", Op: "cancel", Tick: r.Tick + 1, ID: id})
}

// ReturnReservation schedules an early return of the remaining window.
func (r *Room) ReturnReservation(id string) {
	r.resvInbox = append(r.resvInbox, ResvEvent{Type: "resv", Op: "return", Tick: r.Tick + 1, ID: id})
}

// GetReservation returns the authoritative view of one request, or nil.
func (r *Room) GetReservation(id string) *ReservationView {
	res := r.resv[id]
	if res == nil {
		return nil
	}
	v := reservationViewOf(res)
	return &v
}

// ReservationKnown reports whether a request id is already taken —
// either applied or still sitting in the intake inbox — so a duplicate
// submit is acknowledged idempotently even before the next boundary.
func (r *Room) ReservationKnown(id string) bool {
	if r.resv[id] != nil {
		return true
	}
	for _, op := range r.resvInbox {
		if op.Op == "submit" && op.Request != nil && op.Request.ID == id {
			return true
		}
	}
	return false
}

// HasReservationActivity reports whether the reservation subsystem still
// needs the clock to advance (inbox ops, pending requests, or granted
// reservations with future state transitions).
func (r *Room) HasReservationActivity() bool {
	if len(r.resvInbox) > 0 {
		return true
	}
	for _, id := range r.resvOrder {
		switch r.resv[id].State {
		case "pending", "approved", "active":
			return true
		}
	}
	return false
}

// DrainResvEvents returns the derived verdict events produced since the
// last call; the intake layer journals them after the tick record.
func (r *Room) DrainResvEvents() []ResvEvent {
	ev := r.resvEvents
	r.resvEvents = nil
	return ev
}

func (r *Room) emitResvVerdict(res *Reservation, reason string) {
	r.resvEvents = append(r.resvEvents, ResvEvent{
		Type: "resv", Op: "verdict", Tick: r.Tick, ID: res.ID,
		State: res.State, Reason: reason, Conflicts: res.Conflicts,
	})
}

// applyResvInbox applies the intake operations scheduled for the current
// tick, in journal order.
func (r *Room) applyResvInbox() {
	inbox := r.resvInbox
	r.resvInbox = nil
	for _, op := range inbox {
		switch op.Op {
		case "submit":
			req := op.Request
			if req == nil || r.resv[req.ID] != nil {
				continue // duplicate delivery: identity preserved, no re-consumption
			}
			res := &Reservation{
				ReservationRequest: *req,
				SubmitTick:         r.Tick,
				State:              "pending",
			}
			res.Chars = append([]string(nil), req.Chars...)
			res.Regions = append([]string(nil), req.Regions...)
			r.resv[res.ID] = res
			r.resvOrder = append(r.resvOrder, res.ID)
			if !r.validReservation(res) {
				res.State = "rejected"
				res.Reason = "invalid-request"
				r.emitResvVerdict(res, res.Reason)
			}
		case "cancel":
			res := r.resv[op.ID]
			if res == nil {
				continue
			}
			if res.State == "pending" || res.State == "approved" {
				// Cancelled before start: frees any reserved capacity.
				res.State = "cancelled"
				res.Reason = ""
				res.Conflicts = nil
				r.emitResvVerdict(res, "")
			} else {
				// Late cancel: explicit no-op, later grantees unaffected.
				r.emitResvVerdict(res, "cancel-ignored")
			}
		case "return":
			res := r.resv[op.ID]
			if res == nil {
				continue
			}
			switch {
			case res.State != "active":
				// Not started (or already closed): a return is not how
				// capacity is released here; explicit no-op.
				r.emitResvVerdict(res, "return-ignored")
			case res.ReturnedTick > 0:
				// Duplicate return: idempotent no-op.
				r.emitResvVerdict(res, "return-duplicate")
			default:
				res.ReturnedTick = r.Tick
				if res.ReturnedTick < res.Start {
					res.ReturnedTick = res.Start
				}
				r.emitResvVerdict(res, "returned")
			}
		}
	}
}

func (r *Room) validReservation(res *Reservation) bool {
	if len(res.Chars) == 0 || len(res.Regions) == 0 || res.End <= res.Start {
		return false
	}
	for _, ch := range res.Chars {
		if r.byID[ch] == nil {
			return false
		}
	}
	for _, rg := range res.Regions {
		if r.regionByID[rg] == nil {
			return false
		}
	}
	return true
}

// advanceReservations applies time-based lifecycle transitions at the
// current tick: approved reservations start, active ones complete (or
// close as released after an early return).
func (r *Room) advanceReservations() {
	for _, id := range r.resvOrder {
		res := r.resv[id]
		switch res.State {
		case "approved":
			if res.Start <= r.Tick {
				res.State = "active"
				r.emitResvVerdict(res, "")
			}
		case "active":
			if res.effectiveEnd() <= r.Tick {
				if res.ReturnedTick > 0 {
					res.State = "released"
				} else {
					res.State = "completed"
				}
				r.emitResvVerdict(res, "")
			}
		}
	}
}

// adjudicateReservations runs one adjudication boundary for all pending
// requests. The order is fully deterministic: effective priority
// (declared + bounded aging bonus), then submit tick, then request id.
// Approval is atomic across all requested regions and characters.
func (r *Room) adjudicateReservations() {
	var pendings []*Reservation
	for _, id := range r.resvOrder {
		if r.resv[id].State == "pending" {
			pendings = append(pendings, r.resv[id])
		}
	}
	if len(pendings) == 0 {
		return
	}
	sort.SliceStable(pendings, func(i, j int) bool {
		pi := pendings[i].Priority + minInt(pendings[i].WindowsWaited, ResvAgingCap)
		pj := pendings[j].Priority + minInt(pendings[j].WindowsWaited, ResvAgingCap)
		if pi != pj {
			return pi > pj
		}
		if pendings[i].SubmitTick != pendings[j].SubmitTick {
			return pendings[i].SubmitTick < pendings[j].SubmitTick
		}
		return pendings[i].ID < pendings[j].ID
	})
	for _, res := range pendings {
		conflicts := r.resvConflicts(res)
		if res.End <= r.Tick {
			// The window fully elapsed while waiting: reject with the
			// actual conflicts that kept it waiting.
			res.State = "rejected"
			res.Reason = "expired"
			res.Conflicts = conflicts
			r.emitResvVerdict(res, res.Reason)
			continue
		}
		if len(conflicts) == 0 {
			res.State = "approved"
			res.Conflicts = nil
			r.emitResvVerdict(res, "")
			continue
		}
		res.WindowsWaited++
		res.Conflicts = conflicts
	}
}

// resvConflicts lists every concrete interval blocking the request:
// region capacity exceeded within the window, or a character already
// committed to an overlapping granted reservation.
func (r *Room) resvConflicts(res *Reservation) []ConflictInterval {
	var out []ConflictInterval
	for _, regionID := range res.Regions {
		spec := r.regionByID[regionID]
		if spec == nil {
			continue
		}
		capacity := spec.Capacity
		if capacity <= 0 {
			capacity = 1
		}
		var overlapping []*Reservation
		for _, otherID := range r.resvOrder {
			other := r.resv[otherID]
			if other.ID == res.ID || !other.granted() || !resvOverlaps(other, res) {
				continue
			}
			if containsString(other.Regions, regionID) {
				overlapping = append(overlapping, other)
			}
		}
		if len(overlapping) >= capacity {
			for _, other := range overlapping {
				out = append(out, ConflictInterval{
					Kind: "region", ID: regionID,
					Start: maxInt64(other.Start, res.Start),
					End:   minInt64(other.effectiveEnd(), res.End),
					By:    other.ID,
				})
			}
		}
	}
	for _, charID := range res.Chars {
		for _, otherID := range r.resvOrder {
			other := r.resv[otherID]
			if other.ID == res.ID || !other.granted() || !resvOverlaps(other, res) {
				continue
			}
			if containsString(other.Chars, charID) {
				out = append(out, ConflictInterval{
					Kind: "char", ID: charID,
					Start: maxInt64(other.Start, res.Start),
					End:   minInt64(other.effectiveEnd(), res.End),
					By:    other.ID,
				})
			}
		}
	}
	return out
}

func resvOverlaps(a, b *Reservation) bool {
	return a.Start < b.End && b.Start < a.effectiveEnd()
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func reservationViewOf(res *Reservation) ReservationView {
	v := ReservationView{
		ID: res.ID, Chars: append([]string(nil), res.Chars...),
		Regions: append([]string(nil), res.Regions...),
		Start:   res.Start, End: res.End, Priority: res.Priority,
		SubmitTick: res.SubmitTick, State: res.State, Reason: res.Reason,
		WindowsWaited: res.WindowsWaited, ReturnedTick: res.ReturnedTick,
	}
	if len(res.Conflicts) > 0 {
		v.Conflicts = append([]ConflictInterval(nil), res.Conflicts...)
	}
	return v
}
