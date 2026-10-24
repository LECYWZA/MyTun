// Package qos implements adaptive fairness scheduling for the multiplexed
// tunnel.
//
// The problem it solves: a single tunnel carries many channels. Without
// scheduling, one bulk transfer (file download, docker pull) fills the send
// queue and a browsing session on another channel becomes unusable — the
// classic "elephant flow starves the mice" failure.
//
// Design
//
//   - Every channel that writes DATA frames passes through Scheduler.Admit,
//     which applies a per-stream token bucket. Blocking here propagates real
//     backpressure to the producer through TCP flow control; shaping
//     downstream would be too late.
//   - Streams are classified from their own traffic: mostly-small packets with
//     a modest duty cycle look interactive, sustained chunky writes look bulk.
//     Classification is refreshed on every control tick, and quiet streams
//     decay back to interactive so the next HTTP request is never punished.
//   - Interactive streams get a protected reserve and a floor rate; bulk
//     streams share the remainder with equal-share max-min fairness.
//   - Capacity comes from an explicit limit or an EWMA peak observer that
//     rises immediately and decays slowly, so one unrestricted run calibrates
//     the tunnel and later multi-stream periods divide that estimate.
//
// The scheduler is off by default; the console exposes a single switch plus an
// optional bandwidth cap.
package qos

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// Mode selects how capacity is decided.
const (
	ModeAuto  = "auto"  // estimate from observed peak throughput
	ModeFixed = "fixed" // Config.TotalKbps is a hard ceiling
)

// Config is the tunable policy.
type Config struct {
	Enabled bool   `json:"enabled"`
	Mode    string `json:"mode"`
	// TotalKbps caps aggregate tunnel throughput. 0 means "auto" in ModeAuto
	// and "fairness only" in ModeFixed.
	TotalKbps int `json:"totalKbps"`
	// InteractivePercent is the share of capacity reserved for interactive
	// streams (default 20).
	InteractivePercent int `json:"interactivePercent"`
	// InteractiveFloorKbps guarantees every interactive stream a minimum rate
	// (default 320 Kbps) so a fresh browsing channel is usable immediately.
	InteractiveFloorKbps int `json:"interactiveFloorKbps"`
	// BurstKB is the token bucket burst per stream (default 64).
	BurstKB int `json:"burstKb"`
}

// DefaultConfig is the policy used when the console enables QoS without tuning.
func DefaultConfig() Config {
	return Config{
		Enabled:              false,
		Mode:                 ModeAuto,
		TotalKbps:            0,
		InteractivePercent:   20,
		InteractiveFloorKbps: 320,
		// A generous burst lets a short page load (tens to hundreds of KB) finish
		// in one shot while the sustained rate stays capped. This is what keeps
		// browsing snappy without letting a download monopolise the link.
		BurstKB: 512,
	}
}

// Stream kinds reported to the console.
const (
	KindInteractive = "interactive"
	KindBulk        = "bulk"
)

// StreamInfo is a read-only snapshot for the UI.
type StreamInfo struct {
	ID          uint32  `json:"id"`
	Role        string  `json:"role"`
	Kind        string  `json:"kind"`
	LimitKbps   float64 `json:"limitKbps"`
	Bytes       int64   `json:"bytes"`
	Packets     int64   `json:"packets"`
	AvgPktBytes float64 `json:"avgPktBytes"`
	Throttled   int64   `json:"throttled"`
	IdleMS      int64   `json:"idleMs"`
}

type stream struct {
	id   uint32
	role string

	mu         sync.Mutex
	tokens     float64 // bytes available
	lastRefill time.Time
	limitBps   float64 // current ceiling, bytes/sec (0 = unlimited)
	target     float64

	winBytes   int64
	winPackets int64
	winSmall   int64
	totalBytes int64
	totalPkts  int64
	lastActive time.Time
	throttled  int64
	kind       string
	upStreak   int // consecutive ticks suggesting bulk
	downStreak int // consecutive ticks suggesting interactive
	dead       atomic.Bool

	wake chan struct{}
}

const (
	// Packets at or below this size look interactive (TCP ACKs, HTTP headers,
	// DNS, small RPCs).
	smallPacketBytes = 1400
	tickInterval     = 200 * time.Millisecond
	// A stream quiet for this long is forgotten.
	idleTimeout = 120 * time.Second
	minLimitBps = 16 * 1024
)

// Scheduler shapes outbound DATA frames.
type Scheduler struct {
	cfg atomic.Value // Config

	mu      sync.Mutex
	streams map[uint32]*stream

	capBps   float64
	obsBps   float64
	// peakBps is the best sustained rate seen recently. Auto mode divides this
	// when streams compete, and it decays only slowly so a burst of real traffic
	// keeps the estimate usable instead of collapsing to the floor.
	peakBps   float64
	peakAt    time.Time
	lastTick time.Time

	stopCh   chan struct{}
	stopOnce sync.Once

	enabled   atomic.Bool
	throttled atomic.Int64
	admitCalls atomic.Int64
	streamsMade atomic.Int64
	enabledSeen atomic.Int64
}

// New builds a scheduler. Call Start to run the control loop.
func New(cfg Config) *Scheduler {
	s := &Scheduler{
		streams:  map[uint32]*stream{},
		stopCh:   make(chan struct{}),
		lastTick: time.Now(),
	}
	s.cfg.Store(normalize(cfg))
	s.enabled.Store(cfg.Enabled)
	return s
}

func normalize(c Config) Config {
	d := DefaultConfig()
	if c.Mode != ModeFixed {
		c.Mode = d.Mode
	}
	if c.InteractivePercent <= 0 || c.InteractivePercent > 90 {
		c.InteractivePercent = d.InteractivePercent
	}
	if c.InteractiveFloorKbps <= 0 {
		c.InteractiveFloorKbps = d.InteractiveFloorKbps
	}
	if c.InteractiveFloorKbps > d.InteractiveFloorKbps*100 {
		c.InteractiveFloorKbps = d.InteractiveFloorKbps * 100
	}
	if c.BurstKB < 64 || c.BurstKB > 8192 {
		c.BurstKB = d.BurstKB
	}
	return c
}

// Start begins the control loop.
func (s *Scheduler) Start() {
	go s.loop()
}

// Close stops the loop and wakes every waiter.
func (s *Scheduler) Close() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		s.mu.Lock()
		for _, st := range s.streams {
			st.dead.Store(true)
		}
		s.streams = map[uint32]*stream{}
		s.mu.Unlock()
	})
}

// Configure swaps the policy at runtime.
func (s *Scheduler) Configure(c Config) {
	c = normalize(c)
	s.cfg.Store(c)
	prev := s.enabled.Swap(c.Enabled)
	// Fixed-mode capacity is known immediately; publish it before the first
	// control tick so new streams inherit a ceiling from their first byte.
	if c.Enabled && c.Mode == ModeFixed && c.TotalKbps > 0 {
		s.mu.Lock()
		s.capBps = float64(c.TotalKbps) * 1000 / 8
		s.mu.Unlock()
	}
	if prev && !c.Enabled {
		// Lift every ceiling and release waiters.
		s.mu.Lock()
		for _, st := range s.streams {
			st.mu.Lock()
			st.limitBps = 0
			st.mu.Unlock()
			select {
			case st.wake <- struct{}{}:
			default:
			}
		}
		s.mu.Unlock()
	}
}

// Enabled reports the switch state.
func (s *Scheduler) Enabled() bool { return s != nil && s.enabled.Load() }

// Config returns the active policy.
func (s *Scheduler) Config() Config {
	if v, ok := s.cfg.Load().(Config); ok {
		return v
	}
	return DefaultConfig()
}

// Register records a channel role so the classifier can bias proxy/chat
// channels toward interactive.
func (s *Scheduler) Register(id uint32, role string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if st, ok := s.streams[id]; ok {
		st.mu.Lock()
		st.role = role
		st.mu.Unlock()
	} else {
		s.streams[id] = newStream(id, role, s.Config(), s.capBps, len(s.streams))
	}
	s.mu.Unlock()
}

// Unregister drops per-channel state.
func (s *Scheduler) Unregister(id uint32) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if st, ok := s.streams[id]; ok {
		st.dead.Store(true)
		delete(s.streams, id)
	}
	s.mu.Unlock()
}

func newStream(id uint32, role string, cfg Config, capBps float64, active int) *stream {
	now := time.Now()
	st := &stream{
		id:         id,
		role:       role,
		tokens:     float64(cfg.BurstKB) * 1024,
		lastRefill: now,
		lastActive: now,
		kind:       KindInteractive,
		wake:       make(chan struct{}, 1),
	}
	// A new channel starts at its fair share of the known ceiling, not at the
	// full tunnel rate. HTTP requests create a fresh channel each time, so the
	// classifier never gets a chance to look at short-lived streams; handing them
	// the fair share up front is what actually prevents a download from starving
	// them. A lone stream still gets the whole tunnel (share == capBps).
	if cfg.Enabled && cfg.Mode == ModeFixed && capBps > 0 {
		share := capBps
		if active > 0 {
			share = capBps / float64(active+1)
		}
		if share < 64*1024 {
			share = 64 * 1024
		}
		st.limitBps = share
		st.kind = KindBulk
	}
	return st
}

// Admit blocks until the stream may send n more bytes. It returns false only
// when the scheduler is shutting down (callers should then send unthrottled).
//
// Writes larger than the burst are split into burst-sized segments: a token
// bucket whose bucket is smaller than the request would otherwise wait forever
// (tokens can never reach the requested size).
func (s *Scheduler) Admit(id uint32, role string, n int) bool {
	if s == nil {
		return true
	}
	s.admitCalls.Add(1)
	if !s.enabled.Load() {
		return true
	}
	s.enabledSeen.Add(1)
	if n <= 0 {
		return true
	}
	st := s.stream(id, role)
	cfg := s.Config()
	burst := float64(cfg.BurstKB) * 1024
	if burst < 1024 {
		burst = 1024
	}

	for n > 0 {
		seg := n
		if float64(seg) > burst {
			seg = int(burst)
		}
		if !s.admitSegment(st, seg, burst) {
			return false
		}
		n -= seg
	}
	return true
}

// admitSegment waits for and consumes seg bytes of budget.
func (s *Scheduler) admitSegment(st *stream, seg int, burst float64) bool {
	need := float64(seg)
	for {
		select {
		case <-s.stopCh:
			return false
		default:
		}

			if st.dead.Load() {
			return true
		}
		now := time.Now()
		st.mu.Lock()
		limit := st.limitBps
		if limit <= 0 {
			st.observeLocked(seg, now)
			st.mu.Unlock()
			return true
		}
		// Refill for elapsed time, capped at the burst.
		if elapsed := now.Sub(st.lastRefill).Seconds(); elapsed > 0 {
			st.tokens += limit * elapsed
			st.lastRefill = now
		}
		if st.tokens > burst {
			st.tokens = burst
		}
		if st.tokens >= need {
			st.tokens -= need
			st.observeLocked(seg, now)
			st.mu.Unlock()
			return true
		}
		deficit := need - st.tokens
		wait := time.Duration(deficit / limit * float64(time.Second))
		if wait < 300*time.Microsecond {
			wait = 300 * time.Microsecond
		}
		if wait > 100*time.Millisecond {
			wait = 100 * time.Millisecond
		}
		st.throttled++
		st.mu.Unlock()
		s.throttled.Add(1)

		timer := time.NewTimer(wait)
		select {
		case <-s.stopCh:
			timer.Stop()
			return false
		case <-st.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (st *stream) observeLocked(n int, now time.Time) {
	st.winBytes += int64(n)
	st.winPackets++
	if n <= smallPacketBytes {
		st.winSmall++
	}
	st.totalBytes += int64(n)
	st.totalPkts++
	st.lastActive = now
}

func (s *Scheduler) stream(id uint32, role string) *stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.streams[id]; ok {
		if role != "" {
			st.mu.Lock()
			st.role = role
			st.mu.Unlock()
		}
		return st
	}
	st := newStream(id, role, s.Config(), s.capBps, len(s.streams))
	s.streams[id] = st
	s.streamsMade.Add(1)
	return st
}

func (s *Scheduler) loop() {
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			s.tick()
		}
	}
}

// tick refreshes classification, ceilings and the capacity estimate.
func (s *Scheduler) tick() {
	now := time.Now()
	cfg := s.Config()

	s.mu.Lock()
	elapsed := now.Sub(s.lastTick).Seconds()
	if elapsed <= 0 {
		elapsed = tickInterval.Seconds()
	}
	s.lastTick = now

	streams := make([]*stream, 0, len(s.streams))
	for id, st := range s.streams {
		st.mu.Lock()
		idle := now.Sub(st.lastActive) > idleTimeout
		st.mu.Unlock()
		if idle {
			st.dead.Store(true)
			delete(s.streams, id)
			continue
		}
		streams = append(streams, st)
	}
	s.mu.Unlock()

	if len(streams) == 0 {
		s.mu.Lock()
		s.obsBps = 0
		if cfg.Mode == ModeFixed && cfg.TotalKbps > 0 {
			s.capBps = float64(cfg.TotalKbps) * 1000 / 8
		} else {
			// Idle: let the estimate relax slowly. Decaying fast (the old
			// 0.995/tick) collapsed it within seconds and then capped the next
			// transfer at an absurdly low number.
			if time.Since(s.peakAt) > 5*time.Minute {
				s.peakBps *= 0.9995
			}
			s.capBps = s.peakBps
			if s.capBps < 256*1024 {
				s.capBps = 256 * 1024
			}
		}
		s.mu.Unlock()
		return
	}

	var totalBps float64
	interactive := make([]*stream, 0, len(streams))
	bulk := make([]*stream, 0, len(streams))

	// Probing signal: if any stream is currently held back by a ceiling, it is
	// worth testing whether the link can do more.
	throttledAny := false
	for _, st := range streams {
		st.mu.Lock()
		if st.limitBps > 0 {
			throttledAny = true
		}
		st.mu.Unlock()
		if throttledAny {
			break
		}
	}

	// Capacity first: classification compares a stream's rate against it.
	s.mu.Lock()
	if cfg.Mode == ModeFixed && cfg.TotalKbps > 0 {
		s.capBps = float64(cfg.TotalKbps) * 1000 / 8
	} else {
		// Learn from the best rate we have actually carried; probe upwards while
		// every stream is being held back so the estimate can follow a link that
		// got faster.
		if totalObs := s.obsBps; totalObs > s.peakBps {
			s.peakBps = totalObs
			s.peakAt = time.Now()
		}
		if throttledAny && s.peakBps > 0 {
			// Probe upwards quickly: 2%/tick took over a minute to climb from a
			// stale low estimate to a fast link, which felt like "auto is slow".
			s.peakBps *= 1.15
		}
		if time.Since(s.peakAt) > 5*time.Minute {
			s.peakBps *= 0.9995
		}
		s.capBps = s.peakBps
		if cfg.TotalKbps > 0 {
			if maxBps := float64(cfg.TotalKbps) * 1000 / 8; s.capBps > maxBps {
				s.capBps = maxBps
			}
		}
		if s.capBps < 256*1024 {
			s.capBps = 256 * 1024
		}
	}
	capNow := s.capBps
	s.mu.Unlock()

	for _, st := range streams {
		st.mu.Lock()
		rate := float64(st.winBytes) / elapsed
		pkts := st.winPackets
		small := st.winSmall
		role := st.role
		totalBps += rate

		smallRatio := 0.0
		if pkts > 0 {
			smallRatio = float64(small) / float64(pkts)
		}
		ratio := 0.0
		if capNow > 0 {
			ratio = rate / capNow
		}

		// Bulk signals: consuming a large slice of capacity, or sending chunky
		// frames at a meaningful rate. Interactive signals: tiny packets, or
		// traffic that barely touches the pipe.
		// Thresholds have a deliberate gap (0.10 demote / 0.30 promote) so a
		// stream that is already being shaped at its fair share never oscillates
		// between classes.
		bulkSignal := ratio > 0.30 || (smallRatio < 0.5 && ratio > 0.12)
		interSignal := smallRatio >= 0.85 || ratio < 0.10

		if role == "msg" {
			st.kind = KindInteractive
			st.upStreak, st.downStreak = 0, 0
		} else if pkts == 0 {
			if st.kind == "" {
				st.kind = KindInteractive
			}
		} else if st.kind == KindBulk {
			// Hysteresis: a bulk stream must look quiet for ~10 ticks (2s) before
			// it may claim the interactive reserve again.
			if interSignal && ratio < 0.10 {
				st.downStreak++
				if st.downStreak >= 3 {
					st.kind = KindInteractive
					st.downStreak, st.upStreak = 0, 0
				}
			} else {
				st.downStreak = 0
			}
		} else {
			if bulkSignal {
				st.upStreak++
				if st.upStreak >= 2 {
					st.kind = KindBulk
					st.upStreak, st.downStreak = 0, 0
				}
			} else {
				st.upStreak = 0
			}
		}
		st.winBytes = 0
		st.winPackets = 0
		st.winSmall = 0
		kind := st.kind
		st.mu.Unlock()

		if kind == KindInteractive {
			interactive = append(interactive, st)
		} else {
			bulk = append(bulk, st)
		}
	}

	s.mu.Lock()
	s.obsBps = totalBps
	capBps := s.capBps
	s.mu.Unlock()

	if !cfg.Enabled || (len(streams) == 1 && cfg.Mode != ModeFixed) {
		// Auto mode with no contention: let TCP decide the real rate. Capping a
		// single stream at an *estimated* capacity is what made "auto" feel slow.
		// An explicit fixed cap is the user's instruction, so it always applies.
		for _, st := range streams {
			st.mu.Lock()
			st.limitBps = 0
			st.mu.Unlock()
		}
		return
	}

	reserveFrac := float64(cfg.InteractivePercent) / 100
	reserve := capBps * reserveFrac
	if len(interactive) == 0 {
		reserve = 0
	}
	floor := float64(cfg.InteractiveFloorKbps) * 1000 / 8

	var interactiveShare float64
	if n := len(interactive); n > 0 {
		interactiveShare = reserve / float64(n)
		if interactiveShare < floor {
			interactiveShare = floor
		}
		if interactiveShare > capBps {
			interactiveShare = capBps
		}
	}

	remaining := capBps - interactiveShare*float64(len(interactive))
	if remaining < 0 {
		remaining = 0
	}
	var bulkShare float64
	if n := len(bulk); n > 0 {
		bulkShare = remaining / float64(n)
		if bulkShare < minLimitBps {
			bulkShare = minLimitBps
		}
	}

	apply := func(st *stream, target float64, unlimited bool) {
		st.mu.Lock()
		if unlimited {
			st.limitBps = 0
		} else {
			old := st.limitBps
			if old <= 0 {
				old = target
			}
			// Smooth to avoid per-tick oscillation.
			st.limitBps = old*0.6 + target*0.4
		}
		st.mu.Unlock()
		if !st.dead.Load() {
			select {
			case st.wake <- struct{}{}:
			default:
			}
		}
	}

	// A lone interactive stream with an empty bulk set should not be capped:
	// there is nothing to protect.
	for _, st := range interactive {
		if len(bulk) == 0 {
			apply(st, interactiveShare, true)
		} else {
			apply(st, interactiveShare, false)
		}
	}
	for _, st := range bulk {
		apply(st, bulkShare, false)
	}
}

// Stats returns the current per-stream view plus aggregate capacity numbers.
func (s *Scheduler) Stats() map[string]any {
	if s == nil {
		return map[string]any{"enabled": false}
	}
	now := time.Now()
	s.mu.Lock()
	capBps := s.capBps
	obs := s.obsBps
	streams := make([]*stream, 0, len(s.streams))
	for _, st := range s.streams {
		streams = append(streams, st)
	}
	s.mu.Unlock()

	out := make([]StreamInfo, 0, len(streams))
	for _, st := range streams {
		st.mu.Lock()
		avg := 0.0
		if st.totalPkts > 0 {
			avg = float64(st.totalBytes) / float64(st.totalPkts)
		}
		out = append(out, StreamInfo{
			ID: st.id, Role: st.role, Kind: st.kind,
			LimitKbps: math.Round(st.limitBps * 8 / 1000),
			Bytes: st.totalBytes, Packets: st.totalPkts,
			AvgPktBytes: avg, Throttled: st.throttled,
			IdleMS: now.Sub(st.lastActive).Milliseconds(),
		})
		st.mu.Unlock()
	}
	// Cap the report so a busy tunnel cannot flood the console every tick.
	if len(out) > 50 {
		out = out[:50]
	}
	return map[string]any{
		"enabled":      s.Enabled(),
		"streamCount":  len(streams),
		"config":       s.Config(),
		"capacityKbps": math.Round(capBps * 8 / 1000),
		"observedKbps": math.Round(obs * 8 / 1000),
		"streams":      out,
		"throttled":    s.throttled.Load(),
		"admitCalls":   s.admitCalls.Load(),
		"enabledSeen":  s.enabledSeen.Load(),
		"streamsMade":  s.streamsMade.Load(),
	}
}
