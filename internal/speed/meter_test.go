package speed

import (
	"testing"
	"time"
)

// TestMeterSamples verifies the meter turns counter growth into a rate and that
// the series keeps the newest samples in order.
func TestMeterSamples(t *testing.T) {
	var up, down int64
	m := New(func() (int64, int64) { return up, down })
	m.started = true
	m.lastUp, m.lastDn = 0, 0

	for i := 0; i < 10; i++ {
		up += 1000
		down += 2000
		m.sample()
	}
	cu, cd := m.Current()
	if cu <= 0 || cd <= 0 {
		t.Fatalf("current should be positive, got up=%v down=%v", cu, cd)
	}
	b := m.Series(5*time.Minute, 20)
	if len(b.Down) == 0 {
		t.Fatal("series should not be empty")
	}
	// newest sample is last
	last := b.Down[len(b.Down)-1]
	if last < 0 {
		t.Fatalf("unexpected negative rate %v", last)
	}
}

// TestMeterHandlesCounterReset ensures a process restart (counter going
// backwards) is treated as zero instead of a huge negative spike.
func TestMeterHandlesCounterReset(t *testing.T) {
	var total int64 = 1 << 40
	m := New(func() (int64, int64) { return total, total })
	m.started = true
	m.lastUp, m.lastDn = total, total
	total = 10 // counter reset
	m.sample()
	up, down := m.Current()
	if up < 0 || down < 0 {
		t.Fatalf("reset produced negative rate: %v %v", up, down)
	}
}

// TestSeriesWindowBounded checks the series never returns more points than asked
// for and that bucket width is sane.
func TestSeriesWindowBounded(t *testing.T) {
	m := New(func() (int64, int64) { return 0, 0 })
	m.started = true
	for i := 0; i < 120; i++ {
		m.sample()
	}
	b := m.Series(30*time.Minute, 60)
	if len(b.Down) > 60 || len(b.Up) > 60 {
		t.Fatalf("too many points: %d", len(b.Down))
	}
	if b.Seconds < 1 {
		t.Fatalf("bucket seconds = %d", b.Seconds)
	}
}
