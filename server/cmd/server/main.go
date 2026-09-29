// Command server runs the authoritative game service.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"gameserver/internal/engine"
	"gameserver/internal/replay"
	"gameserver/internal/store"
)

type Server struct{ eng *engine.Engine }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	writeJSON(w, map[string]string{"error": msg})
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	room, t1, t2, err := s.eng.CreateRoom()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"roomId":         room.ID,
		"tokens":         map[string]string{"alpha": t1, "beta": t2},
		"chars":          room.CharIDs,
		"tickIntervalMs": 100,
	})
}

func (s *Server) roomFromReq(r *http.Request) (*engine.Room, string) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/rooms/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return nil, ""
	}
	return s.eng.Get(parts[0]), parts[0]
}

// tokenFor resolves the session token from header or query.
func tokenFor(r *http.Request) string {
	if t := r.Header.Get("X-Session-Token"); t != "" {
		return t
	}
	return r.URL.Query().Get("token")
}

func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	room, _ := s.roomFromReq(r)
	if room == nil {
		writeErr(w, 404, "room not found")
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
		writeErr(w, 400, "token required")
		return
	}
	charID, ok := room.Bind(req.Token)
	if !ok {
		// Room full. A foreign/reused token cannot take over either character.
		writeErr(w, 409, "room is full; cannot bind to another character")
		return
	}
	writeJSON(w, map[string]any{
		"roomId": room.ID, "charId": charID,
		"chars": room.CharIDs, "tickIntervalMs": 100,
		"snapshot": room.Snapshot(),
	})
}

func (s *Server) input(w http.ResponseWriter, r *http.Request) {
	room, _ := s.roomFromReq(r)
	if room == nil {
		writeErr(w, 404, "room not found")
		return
	}
	token := tokenFor(r)
	if room.Authenticate(token) == "" {
		writeErr(w, 401, "unauthorized session")
		return
	}
	var d store.InputData
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	d.DirX = clampDir(d.DirX)
	d.DirY = clampDir(d.DirY)
	res := room.SubmitInput(token, d)
	if res.CharID == "" {
		writeErr(w, 401, "unauthorized session")
		return
	}
	writeJSON(w, res)
}

func clampDir(v int64) int64 {
	if v > 0 {
		return 1
	}
	if v < 0 {
		return -1
	}
	return 0
}

func (s *Server) impulse(w http.ResponseWriter, r *http.Request) {
	room, _ := s.roomFromReq(r)
	if room == nil {
		writeErr(w, 404, "room not found")
		return
	}
	if room.Authenticate(tokenFor(r)) == "" {
		writeErr(w, 401, "unauthorized session")
		return
	}
	var d store.ImpulseData
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if _, ok := room.Snapshot().Chars[d.CharID]; !ok {
		writeErr(w, 400, "unknown character")
		return
	}
	d.DirX, d.DirY = clampDir(d.DirX), clampDir(d.DirY)
	room.ScheduleImpulse(d)
	writeJSON(w, map[string]string{"status": "scheduled"})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	room, _ := s.roomFromReq(r)
	if room == nil {
		writeErr(w, 404, "room not found")
		return
	}
	if r.URL.Query().Get("replay") == "1" {
		events := room.Events()
		frames, err := replay.Run(events)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		hashes := make([]string, len(frames))
		mismatchTicks := []int64{}
		loggedHash := ""
		for i, f := range frames {
			hashes[i] = f.State.Hash()
			if !f.Match && f.Tick > 0 {
				mismatchTicks = append(mismatchTicks, f.Tick)
			}
		}
		// Consistent verdict: compare the rebuilt final state against the last
		// checkpoint recorded IN THE SAME event log (snapshot is read separately
		// and may be a tick newer under the live loop).
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Kind == "checkpoint" {
				var cp store.CheckpointData
				_ = json.Unmarshal(events[i].Data, &cp)
				loggedHash = cp.Hash
				break
			}
		}
		liveHash := ""
		if n := len(frames); n > 0 {
			liveHash = frames[n-1].State.Hash()
		}
		writeJSON(w, map[string]any{
			"events": events, "tickHashes": hashes, "mismatchTicks": mismatchTicks,
			"replayedFrames": len(frames), "liveHash": liveHash, "loggedHash": loggedHash,
			"currentHash": room.Snapshot().Hash,
		})
		return
	}
	writeJSON(w, map[string]any{"events": room.Events(), "snapshot": room.Snapshot()})
}

// stream is the SSE endpoint. Last-Event-ID (or ?lastEventId=) lets a client
// that briefly dropped receive everything it missed on reconnect.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	room, _ := s.roomFromReq(r)
	if room == nil {
		writeErr(w, 404, "room not found")
		return
	}
	token := tokenFor(r)
	if room.Authenticate(token) == "" {
		writeErr(w, 401, "unauthorized session")
		return
	}
	last := int64(0)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		last, _ = strconv.ParseInt(strings.TrimPrefix(v, "e"), 10, 64)
	}
	if v := r.URL.Query().Get("lastEventId"); v != "" {
		last, _ = strconv.ParseInt(v, 10, 64)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	f, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	_, sub, cancel := room.Subscribe(last)
	defer cancel()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case b, ok := <-sub:
			if !ok {
				return
			}
			if b.Frame != nil {
				data, _ := json.Marshal(map[string]any{"type": "frame", "frame": b.Frame})
				fmt.Fprintf(w, "data: %s\n\n", data)
			} else {
				data, _ := json.Marshal(map[string]any{"type": "event", "event": b.Event})
				fmt.Fprintf(w, "id: e%d\ndata: %s\n\n", b.Event.ID, data)
			}
			f.Flush()
		}
	}
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dataDir := flag.String("data", "./data", "event log directory")
	flag.Parse()
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	s := &Server{eng: engine.New(*dataDir)}
	mux.HandleFunc("POST /api/rooms", s.createRoom)
	mux.HandleFunc("POST /api/rooms/{id}/join", s.join)
	mux.HandleFunc("POST /api/rooms/{id}/inputs", s.input)
	mux.HandleFunc("POST /api/rooms/{id}/impulse", s.impulse)
	mux.HandleFunc("GET /api/rooms/{id}/events", s.events)
	mux.HandleFunc("GET /api/rooms/{id}/stream", s.stream)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"})
	})
	log.Printf("authoritative server listening on %s (events in %s)", *addr, *dataDir)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
