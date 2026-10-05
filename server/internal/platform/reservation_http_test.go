package platform

import (
	"encoding/json"
	"strings"
	"testing"
)

// submitResv posts a group reservation and decodes the response.
func submitResv(t *testing.T, c testClient, roomID, body string) (int, reservationResponse) {
	t.Helper()
	rw := c.do("POST", "/api/rooms/"+roomID+"/reservations", strings.NewReader(body))
	var resp reservationResponse
	_ = json.Unmarshal(rw.Body.Bytes(), &resp)
	return rw.Code, resp
}

// End-to-end: group reservation lifecycle over HTTP — atomic approval,
// idempotent duplicate submission, cancel before start, replay match.
func TestReservationHTTPLifecycle(t *testing.T) {
	c, srv := newTestServer(t)
	info := createRoom(t, c)
	c1 := info.Chars[0].ID
	c2 := info.Chars[1].ID

	snap := func() int64 {
		rw := c.do("GET", "/api/rooms/"+info.ID, nil)
		var ri roomInfoResponse
		if err := json.Unmarshal(rw.Body.Bytes(), &ri); err != nil {
			t.Fatal(err)
		}
		return ri.Snapshot.Tick
	}
	start := snap() + 10

	// Party of two across hall (cap 2) and vault (cap 1): vault cannot
	// fit the party, so the whole request must stay pending — no region
	// is half-approved.
	code, resp := submitResv(t, c, info.ID, `{"requestId":"grp1","charIds":["`+c1+`","`+c2+`"],"regions":["hall","vault"],"start":`+itoa(start)+`,"end":`+itoa(start+30)+`,"priority":3}`)
	if code != 201 || resp.Reservation.Phase != "pending" {
		t.Fatalf("grp1 should be pending (vault too small), got %d %+v", code, resp)
	}
	if len(resp.Reservation.Conflicts) == 0 {
		t.Fatal("grp1 should carry its actual conflict intervals")
	}

	// Duplicate submission of the same requestId: idempotent, no second
	// request, no extra capacity consumed.
	code, resp2 := submitResv(t, c, info.ID, `{"requestId":"grp1","charIds":["`+c1+`","`+c2+`"],"regions":["hall","vault"],"start":`+itoa(start)+`,"end":`+itoa(start+30)+`,"priority":3}`)
	if code != 200 || resp2.Status != "duplicate" {
		t.Fatalf("duplicate submission should be an idempotent 200, got %d %+v", code, resp2)
	}

	// Hall-only for the same party fits (cap 2) and is approved.
	code, resp = submitResv(t, c, info.ID, `{"requestId":"grp2","charIds":["`+c1+`","`+c2+`"],"regions":["hall"],"start":`+itoa(start)+`,"end":`+itoa(start+30)+`,"priority":1}`)
	if code != 201 || resp.Reservation.Phase != "approved" {
		t.Fatalf("grp2 should be approved, got %d %+v", code, resp)
	}

	// Cancel before start frees the slot.
	rw := c.do("POST", "/api/rooms/"+info.ID+"/reservations/grp2/cancel", nil)
	var cancelled reservationResponse
	_ = json.Unmarshal(rw.Body.Bytes(), &cancelled)
	if cancelled.Reservation.Phase != "cancelled" {
		t.Fatalf("grp2 should be cancelled, got %+v", cancelled.Reservation)
	}

	// A second cancel is an explicit no-op (stays cancelled, no error).
	rw = c.do("POST", "/api/rooms/"+info.ID+"/reservations/grp2/cancel", nil)
	if rw.Code != 200 {
		t.Fatalf("repeated cancel should be an idempotent 200, got %d", rw.Code)
	}

	// Replay from the journal must reproduce every decision.
	rw = c.do("POST", "/api/rooms/"+info.ID+"/replay", nil)
	var rep replayResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.Match {
		t.Fatalf("replay diverged at tick %d: %v", rep.DivergedAt, rep.Errors)
	}

	// The room reloads from its journal after a restart with the same
	// closed verdicts (a fresh manager simulates a process restart).
	mgr2 := NewManager(srv.mgr.dir)
	if err := mgr2.LoadRoom(info.ID); err != nil {
		t.Fatalf("reload: %v", err)
	}
	ri, err := mgr2.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	v1, ok1 := ri.reservationView("grp1")
	v2, ok2 := ri.reservationView("grp2")
	if !ok1 || !ok2 || v1.Phase != "pending" || v2.Phase != "cancelled" {
		t.Fatalf("closed verdicts must survive reload, got %+v %+v", v1, v2)
	}
}

func itoa(v int64) string {
	return strings.TrimSpace(jsonNumber(v))
}

func jsonNumber(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
