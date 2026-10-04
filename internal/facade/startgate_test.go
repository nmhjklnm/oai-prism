package facade

import (
	"testing"
	"time"
)

// 同一瞬间到的三次发起依次领到 0、gap、2·gap；别的账号不受影响；空闲够久后不用等。
func TestStartGate_SpacesPerAccount(t *testing.T) {
	g := newStartGate()
	now := time.Unix(1000, 0)
	gap := 3 * time.Second
	for i, want := range []time.Duration{0, gap, 2 * gap} {
		if got := g.reserve("z1", gap, now); got != want {
			t.Fatalf("第 %d 次等待 = %s，应为 %s", i+1, got, want)
		}
	}
	if got := g.reserve("z2", gap, now); got != 0 {
		t.Fatalf("另一个账号不应排在 z1 后面: %s", got)
	}
	if got := g.reserve("z1", gap, now.Add(time.Minute)); got != 0 {
		t.Fatalf("空闲够久后应立即发起: %s", got)
	}
	if got := g.reserve("z1", 0, now); got != 0 {
		t.Fatal("start_gap 为 0 时不错开")
	}
}
