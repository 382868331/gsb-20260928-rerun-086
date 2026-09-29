package replay

import (
	"path/filepath"
	"testing"

	"gameserver/internal/engine"
	"gameserver/internal/store"
)

// TestReplayMatchesLive verifies the core event-sourcing guarantee: rebuilding
// the world from the saved event log yields the same hashes as the live run.
func TestReplayMatchesLive(t *testing.T) {
	dir := t.TempDir()
	eng := engine.New(dir)
	room, tokenA, tokenB, err := eng.CreateRoom()
	if err != nil {
		t.Fatal(err)
	}
	defer room.Close()

	for i := int64(1); i <= 10; i++ {
		room.SubmitInput(tokenA, store.InputData{Seq: i, DirX: 1})
		room.SubmitInput(tokenB, store.InputData{Seq: i, DirY: 1})
		if i == 5 {
			room.ScheduleImpulse(store.ImpulseData{CharID: "alpha", DirX: -1, DirY: 0})
		}
		// duplicate delivery of an old input must not affect anything
		if i == 7 {
			room.SubmitInput(tokenA, store.InputData{Seq: 3, DirX: 1})
		}
		waitTicks(t, 1)
	}
	waitTicks(t, 1)
	liveHash := room.Snapshot().Hash
	liveTick := room.Snapshot().Tick

	// Re-open the log from disk (fresh process simulation).
	path := filepath.Join(dir, "room-"+room.ID+".jsonl")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := Run(st.Events())
	if err != nil {
		t.Fatal(err)
	}
	last := frames[len(frames)-1]
	if last.State.Hash() != liveHash {
		t.Fatalf("replay hash %s != live hash %s", last.State.Hash(), liveHash)
	}
	if last.State.Tick != liveTick {
		t.Fatalf("replay tick %d != live tick %d", last.State.Tick, liveTick)
	}
	// replaying twice is stable
	frames2, _ := Run(st.Events())
	if frames2[len(frames2)-1].State.Hash() != last.State.Hash() {
		t.Fatal("replay is not stable across runs")
	}
}
