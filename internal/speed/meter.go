package speed

import (
	"math"
	"sync"
	"time"
)

// SampleRate is how often the meter reads the byte counters.
const SampleRate = time.Second

// capacity is how many one-second samples we keep (30 minutes).
const capacity = 1800

// Meter samples cumulative tunnel byte counters once per second and keeps a
// rolling window so the console can draw live 5/15/30 minute speed graphs.
//
// It never touches the data path: the counters it reads are plain atomics that
// the mux already maintains, so sampling costs one atomic load per second.
type Meter struct {
	mu      sync.Mutex
	readFn  func() (up, down int64)
	up      [capacity]float64 // bytes/sec, ring buffer
	down    [capacity]float64
	head    int // index of the newest sample
	count   int
	lastUp  int64
	lastDn  int64
	started bool
	stop    chan struct{}
	once    sync.Once

	// smoothed current speed (exponential moving average) so the readout does
	// not flicker on every single sample
	emaUp, emaDn float64
}

// New creates a meter reading the supplied counters.
func New(read func() (up, down int64)) *Meter {
	return &Meter{readFn: read, stop: make(chan struct{})}
}

// Start begins sampling. Safe to call once.
func (m *Meter) Start() {
	m.once.Do(func() {
		m.mu.Lock()
		m.lastUp, m.lastDn = m.readFn()
		m.started = true
		m.mu.Unlock()
		go m.loop()
	})
}

// Stop halts sampling.
func (m *Meter) Stop() {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
}

func (m *Meter) loop() {
	t := time.NewTicker(SampleRate)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.sample()
		}
	}
}

func (m *Meter) sample() {
	up, dn := m.readFn()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started {
		m.lastUp, m.lastDn = up, dn
		m.started = true
		return
	}
	du := up - m.lastUp
	dd := dn - m.lastDn
	// Counter reset (process restart) or a backwards jump: treat as zero.
	if du < 0 {
		du = 0
	}
	if dd < 0 {
		dd = 0
	}
	m.lastUp, m.lastDn = up, dn

	m.head = (m.head + 1) % capacity
	m.up[m.head] = float64(du)
	m.down[m.head] = float64(dd)
	if m.count < capacity {
		m.count++
	}
	// Smooth the live readout: 0.25 weight keeps it responsive without
	// flickering between samples.
	m.emaUp = m.emaUp*0.75 + float64(du)*0.25
	m.emaDn = m.emaDn*0.75 + float64(dd)*0.25
}

// Current returns the smoothed live speed in bytes per second.
func (m *Meter) Current() (up, down float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.emaUp, m.emaDn
}

// Buckets is one point of a chart series.
type Buckets struct {
	// Seconds is the width of each bucket.
	Seconds int `json:"bucketSeconds"`
	// Up and Down hold one value per bucket, oldest first, in bytes/sec.
	Up   []float64 `json:"up"`
	Down []float64 `json:"down"`
	// Peak is the maximum value across both directions, for scaling.
	Peak float64 `json:"peak"`
}

// Series builds a chart series for the requested window, aggregating the
// one-second samples into at most points buckets.
func (m *Meter) Series(window time.Duration, points int) Buckets {
	if points <= 0 {
		points = 60
	}
	total := int(window / SampleRate)
	if total > capacity {
		total = capacity
	}
	bucket := total / points
	if bucket < 1 {
		bucket = 1
	}

	m.mu.Lock()
	n := m.count
	if total < n {
		n = total
	}
	ups := make([]float64, 0, points)
	dns := make([]float64, 0, points)
	peak := 0.0
	// Walk backwards from the newest sample, then reverse.
	accU, accD, accN := 0.0, 0.0, 0
	for i := 0; i < n; i++ {
		idx := (m.head - i + capacity*2) % capacity
		accU += m.up[idx]
		accD += m.down[idx]
		accN++
		if accN == bucket || i == n-1 {
			// rounded to whole bytes/sec: the UI never shows more precision and
			// it roughly halves the JSON payload
			ups = append(ups, math.Round(accU/float64(accN)))
			dns = append(dns, math.Round(accD/float64(accN)))
			if accU/float64(accN) > peak {
				peak = accU / float64(accN)
			}
			if accD/float64(accN) > peak {
				peak = accD / float64(accN)
			}
			accU, accD, accN = 0, 0, 0
		}
	}
	m.mu.Unlock()

	// newest-first -> oldest-first
	for i, j := 0, len(ups)-1; i < j; i, j = i+1, j-1 {
		ups[i], ups[j] = ups[j], ups[i]
		dns[i], dns[j] = dns[j], dns[i]
	}
	return Buckets{Seconds: bucket, Up: ups, Down: dns, Peak: math.Round(peak)}
}
