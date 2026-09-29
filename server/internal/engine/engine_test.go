package engine

import (
	"testing"
	"time"

	"gameserver/internal/store"
)

func newTestRoom(t *testing.T) (*Room, string, string) {
	t.Helper()
	eng := New(t.TempDir())
	r, ta, tb, err := eng.CreateRoom()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r, ta, tb
}

func TestDuplicateInputAppliedOnce(t *testing.T) {
	r, tokenA, _ := newTestRoom(t)
	r.SubmitInput(tokenA, store.InputData{Seq: 1, DirX: 1})
	r.SubmitInput(tokenA, store.InputData{Seq: 1, DirX: 1}) // duplicate packet
	time.Sleep(150 * time.Millisecond)
	lastSeq := r.Snapshot().Chars["alpha"].LastSeq
	if lastSeq != 1 {
		t.Fatalf("dup input changed lastSeq: %d", lastSeq)
	}
	inputs := 0
	for _, ev := range r.Events() {
		if ev.Kind == "input" {
			inputs++
		}
	}
	if inputs != 1 {
		t.Fatalf("expected 1 stored input event, got %d", inputs)
	}
}

func TestForeignTokenCannotHijack(t *testing.T) {
	r, tokenA, tokenB := newTestRoom(t)
	if got := r.Authenticate(tokenA); got != "alpha" {
		t.Fatalf("tokenA = %q, want alpha", got)
	}
	if got := r.Authenticate(tokenB); got != "beta" {
		t.Fatalf("tokenB = %q, want beta", got)
	}
	if got := r.Authenticate("deadbeef-deadbeef"); got != "" {
		t.Fatalf("forged token bound to %q", got)
	}
	// room already has 2 owners; a third token cannot bind or steal a slot
	if ch, ok := r.Bind("00000000000000000000"); ok || ch != "" {
		t.Fatalf("foreign token took over character %q", ch)
	}
	// original token resumes its own character after "reconnect"
	if ch, ok := r.Bind(tokenA); !ok || ch != "alpha" {
		t.Fatalf("resume failed: %q %v", ch, ok)
	}
}

func TestClientCanOnlyDriveOwnCharacter(t *testing.T) {
	r, tokenA, _ := newTestRoom(t)
	// attempt to move beta with alpha's token; server rewrites CharID to alpha
	r.SubmitInput(tokenA, store.InputData{Seq: 1, DirX: 1, CharID: "beta"})
	time.Sleep(150 * time.Millisecond)
	b := r.Snapshot().Chars["beta"]
	if b.LastSeq != 0 || b.X != 0 {
		t.Fatalf("beta moved by foreign token: x=%d lastSeq=%d", b.X, b.LastSeq)
	}
	a := r.Snapshot().Chars["alpha"]
	if a.LastSeq != 1 {
		t.Fatalf("alpha did not receive its own input: %+v", a)
	}
}
