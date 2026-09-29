// Package replay rebuilds authoritative snapshots purely from the event log.
package replay

import (
	"encoding/json"
	"gameserver/internal/sim"
	"gameserver/internal/store"
)

type Frame struct {
	Tick    int64         `json:"tick"`
	State   *sim.State    `json:"state"`
	Applied []store.Event `json:"appliedEvents"`
	Match   bool          `json:"match"`
}

// Run deterministically rebuilds frames from the event stream. Given the same
// events it always yields the same snapshots and hashes, on any machine.
func Run(events []store.Event) ([]Frame, error) {
	var state *sim.State
	var maxTick int64
	byTick := map[int64][]store.Event{}
	checks := map[int64]string{}
	for _, e := range events {
		switch e.Kind {
		case "room":
			var d store.RoomData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, err
			}
			state = sim.NewState(d.CharIDs)
		case "input", "impulse":
			byTick[e.At] = append(byTick[e.At], e)
		case "checkpoint":
			var d store.CheckpointData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, err
			}
			checks[e.At] = d.Hash
		}
		if e.At > maxTick {
			maxTick = e.At
		}
	}
	if state == nil {
		return nil, nil
	}
	frames := []Frame{{Tick: 0, State: state.Clone(), Match: true}}
	for t := int64(1); t <= maxTick; t++ {
		ev := byTick[t]
		for _, e := range ev {
			switch e.Kind {
			case "input":
				var d store.InputData
				if err := json.Unmarshal(e.Data, &d); err != nil {
					return nil, err
				}
				state.ApplyInput(d.CharID, d.Seq, d.DirX, d.DirY)
			case "impulse":
				var d store.ImpulseData
				if err := json.Unmarshal(e.Data, &d); err != nil {
					return nil, err
				}
				state.ApplyImpulse(d.CharID, d.DirX, d.DirY)
			}
		}
		state.Advance()
		match := true
		if want, ok := checks[t]; ok {
			match = want == state.Hash()
		}
		frames = append(frames, Frame{Tick: t, State: state.Clone(), Applied: ev, Match: match})
	}
	return frames, nil
}
