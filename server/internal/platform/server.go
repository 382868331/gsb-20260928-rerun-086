package platform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"gameplatform/internal/game"
	"gameplatform/internal/store"
)

type Server struct {
	mgr *Manager
}

func NewServer(dataDir string) *Server {
	return &Server{mgr: NewManager(dataDir)}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/rooms", s.createRoom)
	mux.HandleFunc("GET /api/rooms/{id}", s.getRoom)
	mux.HandleFunc("GET /api/rooms/{id}/events", s.events)
	mux.HandleFunc("POST /api/rooms/{id}/inputs", s.postInput)
	mux.HandleFunc("POST /api/rooms/{id}/rules", s.postRules)
	mux.HandleFunc("GET /api/rooms/{id}/journal", s.journal)
	mux.HandleFunc("POST /api/rooms/{id}/replay", s.replay)
	mux.HandleFunc("POST /api/rooms/{id}/reservations", s.postReservation)
	mux.HandleFunc("POST /api/rooms/{id}/reservations/{rid}/cancel", s.cancelReservation)
	mux.HandleFunc("POST /api/rooms/{id}/reservations/{rid}/release", s.releaseReservation)
	return withCommonHeaders(mux)
}

func withCommonHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	_, info, err := s.mgr.CreateRoom()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, info)
}

type roomInfoResponse struct {
	RoomID    string              `json:"roomId"`
	Config    game.Config         `json:"config"`
	Chars     []CharView          `json:"chars"`
	Sessions  []SessionView       `json:"sessions"`
	Snapshot  game.Snapshot       `json:"snapshot"`
	Rules     game.Rules          `json:"rules"`
	Resources []game.ResourceSpec `json:"resources"`
}

func (s *Server) getRoom(w http.ResponseWriter, r *http.Request) {
	ri, err := s.roomOrLoad(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "room not found")
		return
	}
	writeJSON(w, 200, roomInfoResponse{
		RoomID: ri.ID, Config: ri.Cfg, Chars: ri.Views,
		Sessions: ri.Sessions, Snapshot: ri.snapshot(), Rules: ri.rules(),
		Resources: ri.Cfg.Resources,
	})
}

type rulesRequest struct {
	Sealed       [][2]int             `json:"sealed"`
	Costs        map[game.Command]int `json:"costs"`
	ClaimsClosed *bool                `json:"claimsClosed"`
}

// postRules switches the room's action rules to a new version (e.g. seal a
// region or change an action's energy cost). The switch is journaled and
// takes effect at the next tick; inputs formed under older versions keep
// their declared basis and are judged explicitly against the new version.
func (s *Server) postRules(w http.ResponseWriter, r *http.Request) {
	ri, err := s.roomOrLoad(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "room not found")
		return
	}
	var req rulesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	for _, cell := range req.Sealed {
		if cell[0] < 0 || cell[1] < 0 || cell[0] >= ri.Cfg.Width || cell[1] >= ri.Cfg.Height {
			writeErr(w, 400, "sealed cell out of bounds")
			return
		}
	}
	for cmd, cost := range req.Costs {
		switch cmd {
		case game.CmdUp, game.CmdDown, game.CmdLeft, game.CmdRight, game.CmdWait, game.CmdClaim:
		default:
			writeErr(w, 400, "unknown command in costs")
			return
		}
		if cost < 0 {
			writeErr(w, 400, "cost must be >= 0")
			return
		}
	}
	rules := ri.SwitchRules(req.Sealed, req.Costs, req.ClaimsClosed)
	writeJSON(w, 200, rules)
}

type reservationRequest struct {
	RequestID string   `json:"requestId"`
	CharIDs   []string `json:"charIds"`
	Regions   []string `json:"regions"`
	Start     int64    `json:"start"`
	End       int64    `json:"end"`
	Priority  int      `json:"priority"`
}

type reservationResponse struct {
	Status      string        `json:"status"` // created | duplicate
	Reservation game.ResvView `json:"reservation"`
}

// postReservation submits a group multi-region pass reservation. The
// client-generated requestId is the idempotency identity: reconnects and
// duplicate submissions return the existing record unchanged.
func (s *Server) postReservation(w http.ResponseWriter, r *http.Request) {
	ri, err := s.roomOrLoad(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "room not found")
		return
	}
	var req reservationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	if req.RequestID == "" {
		writeErr(w, 400, "requestId is required")
		return
	}
	if len(req.CharIDs) == 0 || len(req.Regions) == 0 {
		writeErr(w, 400, "charIds and regions are required")
		return
	}
	seenChar := map[string]bool{}
	for _, c := range req.CharIDs {
		if seenChar[c] {
			writeErr(w, 400, "duplicate charId")
			return
		}
		seenChar[c] = true
		found := false
		for _, ch := range ri.Chars {
			if ch.ID == c {
				found = true
				break
			}
		}
		if !found {
			writeErr(w, 400, "unknown charId: "+c)
			return
		}
	}
	seenRegion := map[string]bool{}
	for _, g := range req.Regions {
		if seenRegion[g] {
			writeErr(w, 400, "duplicate region")
			return
		}
		seenRegion[g] = true
		found := false
		for _, spec := range ri.Cfg.Regions {
			if spec.ID == g {
				found = true
				break
			}
		}
		if !found {
			writeErr(w, 400, "unknown region: "+g)
			return
		}
	}
	if req.End <= req.Start {
		writeErr(w, 400, "end must be after start")
		return
	}
	if req.Start <= ri.snapshot().Tick {
		writeErr(w, 400, "start must be in the future")
		return
	}
	if req.Priority < 0 || req.Priority > 10 {
		writeErr(w, 400, "priority must be in [0,10]")
		return
	}
	view, status := ri.submitReservation(game.ResvRequest{
		ID: req.RequestID, CharIDs: req.CharIDs, Regions: req.Regions,
		Start: req.Start, End: req.End, Priority: req.Priority,
	})
	code := 201
	if status == "duplicate" {
		// Idempotent acknowledgement of a retried submission.
		code = 200
	}
	writeJSON(w, code, reservationResponse{Status: status, Reservation: view})
}

func (s *Server) reservationOp(w http.ResponseWriter, r *http.Request, op string) {
	ri, err := s.roomOrLoad(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "room not found")
		return
	}
	view, ok := ri.resvOp(op, r.PathValue("rid"))
	if !ok {
		writeErr(w, 404, "reservation not found")
		return
	}
	writeJSON(w, 200, reservationResponse{Status: "ok", Reservation: view})
}

// cancelReservation withdraws a request before its window starts. A late
// cancel (already started) is an explicit no-op: capacity stays with the
// holders and later grantees are unaffected.
func (s *Server) cancelReservation(w http.ResponseWriter, r *http.Request) {
	s.reservationOp(w, r, "cancel")
}

// releaseReservation returns the remaining window of an active
// reservation early, freeing capacity from now on. A duplicate release is
// an idempotent no-op.
func (s *Server) releaseReservation(w http.ResponseWriter, r *http.Request) {
	s.reservationOp(w, r, "release")
}

func (s *Server) roomOrLoad(id string) (*RoomInstance, error) {
	ri, err := s.mgr.Get(id)
	if err == nil {
		return ri, nil
	}
	if err := s.mgr.LoadRoom(id); err != nil {
		return nil, err
	}
	return s.mgr.Get(id)
}

type inputRequest struct {
	SessionID string       `json:"sessionId"`
	CharID    string       `json:"charId"`
	Seq       int64        `json:"seq"`
	Cmd       game.Command `json:"cmd"`
	// Resource is the shared resource a claim targets.
	Resource string `json:"resource"`
	// DependsOn marks this input as relying on the success of an earlier
	// same-session input (0 = independent).
	DependsOn int64 `json:"dependsOn"`
	// RuleVersion is the rules version the client based this input on.
	RuleVersion int `json:"ruleVersion"`
}

func (s *Server) postInput(w http.ResponseWriter, r *http.Request) {
	ri, err := s.roomOrLoad(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "room not found")
		return
	}
	var req inputRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	if req.SessionID == "" || req.CharID == "" {
		writeErr(w, 400, "sessionId and charId are required")
		return
	}
	if req.Seq <= 0 {
		writeErr(w, 400, "seq must be positive")
		return
	}
	switch req.Cmd {
	case game.CmdUp, game.CmdDown, game.CmdLeft, game.CmdRight, game.CmdWait, game.CmdClaim:
	default:
		writeErr(w, 400, "unknown command")
		return
	}
	if req.Cmd == game.CmdClaim {
		found := false
		for _, spec := range ri.Cfg.Resources {
			if spec.ID == req.Resource {
				found = true
				break
			}
		}
		if !found {
			writeErr(w, 400, "unknown resource")
			return
		}
	}
	if req.DependsOn < 0 || req.DependsOn >= req.Seq {
		writeErr(w, 400, "dependsOn must be >= 0 and < seq")
		return
	}
	res := ri.enqueue(game.Input{
		SessionID: req.SessionID, CharID: req.CharID, Seq: req.Seq, Cmd: req.Cmd,
		Resource: req.Resource, DependsOn: req.DependsOn,
		RuleVersion: req.RuleVersion,
	})
	code := 202
	if res.Status == "forbidden" {
		code = 403
	} else if res.Status == "stale" {
		// Idempotent acknowledgement, not an error: a retried duplicate gets
		// the same ack and is never executed twice.
		code = 200
	}
	writeJSON(w, code, res)
}

// events is the SSE stream. Last-Event-ID/?after resume from the in-memory
// ring; afterTick survives a process restart and never re-runs an input.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	ri, err := s.roomOrLoad(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "room not found")
		return
	}
	after := int64(-1)
	afterTick := int64(-1)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			after = n
		}
	}
	if v := r.URL.Query().Get("after"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			after = n
		}
	}
	if v := r.URL.Query().Get("afterTick"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			afterTick = n
			after = -1
		}
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	subID, ch, backlog := ri.subscribe(after, afterTick)
	defer ri.unsubscribe(subID)
	// Backlog first, then live events.
	ping := []byte(": ping\n\n")
	write := func(m pubMsg) bool {
		if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", m.id, m.payload); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	for _, m := range backlog {
		if !write(m) {
			return
		}
	}
	ctx := r.Context()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-ch:
			if !ok || !write(m) {
				return
			}
		case <-ticker.C:
			if _, err := w.Write(ping); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

type replayResponse struct {
	RoomID     string   `json:"roomId"`
	Ticks      int      `json:"ticks"`
	FinalHash  string   `json:"finalHash"`
	LiveFinal  string   `json:"liveFinalHash"`
	Match      bool     `json:"match"`
	DivergedAt int64    `json:"divergedAt,omitempty"`
	Errors     []string `json:"errors,omitempty"`
}

// replay rebuilds state from the saved journal and compares every snapshot
// hash against the hashes recorded during the live run.
func (s *Server) replay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	resp, err := ReplayFromDisk(s.mgr.dir, id)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) journal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	err := store.ReadEntries(s.mgr.dir, id, func(line []byte) error {
		_, err := w.Write(line)
		if err != nil {
			return err
		}
		_, err = w.Write([]byte{'\n'})
		return err
	})
	if err != nil {
		// Headers may already be flushed; best-effort trailing comment.
		_, _ = w.Write([]byte("// read error: " + err.Error() + "\n"))
	}
}

// ReplayFromDisk reads the journal, replays deterministically and verifies
// hashes against the live-recorded chain.
func ReplayFromDisk(dir, id string) (replayResponse, error) {
	var (
		initEntry  store.InitEntry
		events     []game.JournalEvent
		liveHashes []string
	)
	err := store.ReadEntries(dir, id, func(line []byte) error {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			return err
		}
		switch head.Type {
		case "init":
			e, err := store.DecodeInit(line)
			if err != nil {
				return err
			}
			initEntry = e
		case "tick":
			var e struct {
				game.TickRecord
				Hash string `json:"hash"`
			}
			if err := json.Unmarshal(line, &e); err != nil {
				return err
			}
			rec := e.TickRecord
			events = append(events, game.JournalEvent{Tick: &rec})
			liveHashes = append(liveHashes, e.Hash)
		case "rules":
			var e game.RulesRecord
			if err := json.Unmarshal(line, &e); err != nil {
				return err
			}
			rec := e
			events = append(events, game.JournalEvent{Rules: &rec})
		case "round":
			var e game.RoundEvent
			if err := json.Unmarshal(line, &e); err != nil {
				return err
			}
			rev := e
			events = append(events, game.JournalEvent{Round: &rev})
		case "resvreq":
			var e game.ResvRequest
			if err := json.Unmarshal(line, &e); err != nil {
				return err
			}
			req := e
			events = append(events, game.JournalEvent{ResvReq: &req})
		case "resvop":
			var e game.ResvOpRecord
			if err := json.Unmarshal(line, &e); err != nil {
				return err
			}
			rec := e
			events = append(events, game.JournalEvent{ResvOp: &rec})
		case "resv":
			var e game.ResvEvent
			if err := json.Unmarshal(line, &e); err != nil {
				return err
			}
			rev := e
			events = append(events, game.JournalEvent{Resv: &rev})
		}
		return nil
	})
	if err != nil {
		return replayResponse{}, err
	}
	if initEntry.Type == "" {
		return replayResponse{}, fmt.Errorf("no journal for room %s", id)
	}
	var cfg game.Config
	if err := json.Unmarshal(initEntry.Config, &cfg); err != nil {
		return replayResponse{}, err
	}
	var chars []*game.Char
	if err := json.Unmarshal(initEntry.Chars, &chars); err != nil {
		return replayResponse{}, err
	}
	_, snaps, err := game.ReplayJournal(id, cfg, chars, events)
	if err != nil {
		return replayResponse{RoomID: id, Errors: []string{err.Error()}}, nil
	}
	resp := replayResponse{RoomID: id, Ticks: len(snaps), Match: true}
	for i, snap := range snaps {
		if i < len(liveHashes) && snap.Hash != liveHashes[i] {
			resp.Match = false
			if resp.DivergedAt == 0 {
				resp.DivergedAt = snap.Tick
			}
		}
		resp.FinalHash = snap.Hash
	}
	if len(liveHashes) > 0 {
		resp.LiveFinal = liveHashes[len(liveHashes)-1]
	}
	return resp, nil
}
