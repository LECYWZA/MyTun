package qos

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestFixedModeEnforcesCeiling verifies that a single bulk stream cannot exceed
// the configured aggregate rate.
func TestFixedModeEnforcesCeiling(t *testing.T) {
	cfg := Config{Enabled: true, Mode: ModeFixed, TotalKbps: 1000, // 1 Mbps = 125 KB/s
		InteractivePercent: 20, InteractiveFloorKbps: 64, BurstKB: 32}
	s := New(cfg)
	s.Start()
	defer s.Close()

	const chunk = 64 * 1024
	// Warm up: the first ticks still classify the stream as interactive and
	// leave it unshaped, so measurements must start after the policy engages.
	warm := time.Now().Add(900 * time.Millisecond)
	for time.Now().Before(warm) {
		s.Admit(1, "forward", chunk)
	}
	start := time.Now()
	deadline := start.Add(3 * time.Second)
	var total int64
	for time.Now().Before(deadline) {
		s.Admit(1, "forward", chunk)
		atomic.AddInt64(&total, chunk)
	}
	elapsed := time.Since(start).Seconds()
	gotBps := float64(total) / elapsed

	// Allow warm-up (~1s of unshaped classification plus burst) and smoothing.
	maxExpected := 125*1024*1.6 + float64(2*32*1024)
	if gotBps > maxExpected {
		t.Fatalf("rate not enforced: got %.0f B/s, ceiling 125 KiB/s (max allowed %.0f)", gotBps, maxExpected)
	}
	if gotBps < 20*1024 {
		t.Fatalf("over-throttled: got %.0f B/s", gotBps)
	}
	t.Logf("observed %.0f B/s over %.2fs (limit 128000 B/s)", gotBps, elapsed)
}

// TestBulkClassification makes sure a sustained stream is classified bulk and
// gets a finite ceiling.
func TestBulkClassification(t *testing.T) {
	cfg := Config{Enabled: true, Mode: ModeFixed, TotalKbps: 10000,
		InteractivePercent: 20, InteractiveFloorKbps: 128, BurstKB: 64}
	s := New(cfg)
	s.Start()
	defer s.Close()

	s.Register(7, "forward")
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		s.Admit(7, "forward", 64*1024)
	}
	stats := s.Stats()
	streams, _ := stats["streams"].([]StreamInfo)
	if len(streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(streams))
	}
	if streams[0].Kind != KindBulk {
		t.Fatalf("expected bulk classification, got %q", streams[0].Kind)
	}
	if streams[0].LimitKbps <= 0 {
		t.Fatalf("bulk stream should be rate limited, got %v", streams[0].LimitKbps)
	}
	t.Logf("kind=%s limit=%.0f kbps", streams[0].Kind, streams[0].LimitKbps)
}

// TestInteractiveReserve gives an interactive stream a floor while a bulk
// stream is saturating the tunnel.
func TestInteractiveReserve(t *testing.T) {
	cfg := Config{Enabled: true, Mode: ModeFixed, TotalKbps: 8000,
		InteractivePercent: 30, InteractiveFloorKbps: 320, BurstKB: 32}
	s := New(cfg)
	s.Start()
	defer s.Close()

	// bulk stream saturates
	go func() {
		end := time.Now().Add(3 * time.Second)
		for time.Now().Before(end) {
			s.Admit(1, "forward", 64*1024)
		}
	}()
	// interactive stream: small writes
	done := make(chan struct{})
	go func() {
		defer close(done)
		end := time.Now().Add(2 * time.Second)
		for time.Now().Before(end) {
			s.Admit(2, "msg", 512)
			time.Sleep(2 * time.Millisecond)
		}
	}()
	<-done

	stats := s.Stats()
	streams, _ := stats["streams"].([]StreamInfo)
	var found bool
	for _, st := range streams {
		if st.ID == 2 {
			found = true
			if st.LimitKbps < 100 {
				t.Fatalf("interactive stream limit too low: %.0f kbps", st.LimitKbps)
			}
			t.Logf("interactive kind=%s limit=%.0f kbps", st.Kind, st.LimitKbps)
		}
	}
	if !found {
		t.Fatal("interactive stream missing from stats")
	}
}

// TestDisabledIsPassthrough ensures a disabled scheduler never blocks nor
// accounts.
func TestDisabledIsPassthrough(t *testing.T) {
	s := New(Config{Enabled: false})
	s.Start()
	defer s.Close()
	start := time.Now()
	for i := 0; i < 1000; i++ {
		if !s.Admit(1, "forward", 64*1024) {
			t.Fatal("admit returned false while disabled")
		}
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("disabled scheduler blocked: %v", d)
	}
}

// TestConcurrentLifecycle hammers Admit/Unregister/Configure from many
// goroutines. It reproduces the "send on closed channel" panic that a plain
// close(wake) teardown caused when the control tick raced with channel close.
func TestConcurrentLifecycle(t *testing.T) {
	s := New(Config{Enabled: true, Mode: ModeFixed, TotalKbps: 2000,
		InteractivePercent: 20, InteractiveFloorKbps: 64, BurstKB: 32})
	s.Start()
	defer s.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(base uint32) {
			defer wg.Done()
			for j := 0; j < 400; j++ {
				id := base*1000 + uint32(j%20)
				s.Admit(id, "forward", 8192)
				if j%7 == 0 {
					s.Unregister(id)
				}
			}
		}(uint32(i))
	}
	// Toggle policy while streams come and go.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			s.Configure(Config{Enabled: i%2 == 0, Mode: ModeFixed, TotalKbps: 1000 + i*10,
				InteractivePercent: 20, InteractiveFloorKbps: 64, BurstKB: 32})
			time.Sleep(2 * time.Millisecond)
		}
	}()
	go func() {
		time.Sleep(500 * time.Millisecond)
		close(stop)
	}()
	wg.Wait()
	_ = stop
}
