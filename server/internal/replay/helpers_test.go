package replay

import (
	"testing"
	"time"

	"gameserver/internal/engine"
)

func waitTicks(t *testing.T, n int) {
	t.Helper()
	time.Sleep(time.Duration(n)*engine.TickInterval + 20*time.Millisecond)
}
