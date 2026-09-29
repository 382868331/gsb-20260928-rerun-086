package sim

import "testing"

func TestFixedPointDeterminism(t *testing.T) {
	s1 := NewState([]string{"alpha", "beta"})
	s2 := NewState([]string{"alpha", "beta"})
	for i := int64(1); i <= 50; i++ {
		s1.ApplyInput("alpha", i, 1, 0)
		s1.ApplyInput("beta", i, 0, 1)
		s2.ApplyInput("alpha", i, 1, 0)
		s2.ApplyInput("beta", i, 0, 1)
		s1.Advance()
		s2.Advance()
	}
	if s1.Hash() != s2.Hash() {
		t.Fatalf("same inputs diverged: %s vs %s", s1.Hash(), s2.Hash())
	}
	// friction must decay velocity to rest after input stops
	for i := 0; i < 100; i++ {
		s1.Advance()
	}
	a := s1.Chars["alpha"]
	if a.VX != 0 || a.VY != 0 {
		t.Fatalf("expected rest, got v=(%d,%d)", a.VX, a.VY)
	}
}

func TestStaleInputNeverAppliesTwice(t *testing.T) {
	s := NewState([]string{"alpha"})
	s.ApplyInput("alpha", 1, 1, 0)
	s.Advance()
	x1 := s.Chars["alpha"].X
	s.ApplyInput("alpha", 1, 1, 0) // duplicate seq: must be ignored
	s.ApplyInput("alpha", 0, 1, 0) // older seq: must be ignored
	s.Advance()
	x2 := s.Chars["alpha"].X
	if x2 <= x1 {
		t.Fatalf("duplicate input advanced state: %d -> %d", x1, x2)
	}
	// without any new input, friction tick moves only by residual velocity
	if s.Chars["alpha"].LastSeq != 1 {
		t.Fatalf("last seq changed: %d", s.Chars["alpha"].LastSeq)
	}
}

func TestWallsClamp(t *testing.T) {
	s := NewState([]string{"alpha"})
	for i := int64(1); i <= 200; i++ {
		s.ApplyInput("alpha", i, 1, 1)
		s.Advance()
	}
	a := s.Chars["alpha"]
	if a.X != FieldHalf || a.Y != FieldHalf {
		t.Fatalf("not clamped to corner: (%d,%d)", a.X, a.Y)
	}
}
