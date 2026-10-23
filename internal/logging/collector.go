// Package logging keeps an in-memory audit ring and persists records to SQLite.
package logging

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/LECYWZA/mytun/internal/store"
)

// Record is one audit entry shown in the UI.
type Record struct {
	ID          int64  `json:"id"`
	TS          int64  `json:"ts"`
	Module      string `json:"module"`
	Source      string `json:"source"`
	Target      string `json:"target"`
	Action      string `json:"action"`
	NetworkMode string `json:"networkMode"`
	BytesUp     int64  `json:"bytesUp"`
	BytesDown   int64  `json:"bytesDown"`
	DurationMS  int64  `json:"durationMs"`
	Status      string `json:"status"`
	Error       string `json:"error"`
	RuleID      string `json:"ruleId"`
}

// Collector buffers audit records and fans them out to websocket subscribers.
type Collector struct {
	mu   sync.RWMutex
	ring []Record
	max  int
	subs map[int]chan Record
	next int
	seq  atomic.Int64

	st       *store.Store
	writeCh  chan Record
	stopOnce sync.Once
	stopCh   chan struct{}
	dropped  atomic.Int64

	// Persistence throttle: a busy tunnel can emit thousands of audit rows per
	// second. The in-memory ring still shows everything live; only this many
	// rows per second reach SQLite, which keeps the database (and the WAL) small.
	maxPerSec  int
	persisted  atomic.Int64
	throttled  atomic.Int64
	secStart   time.Time
	secCount   int
	secMu      sync.Mutex
}

// New creates a collector. A nil store disables persistence.
func New(st *store.Store, capacity int) *Collector {
	if capacity <= 0 {
		capacity = 5000
	}
	c := &Collector{
		max:       capacity,
		subs:      map[int]chan Record{},
		st:        st,
		writeCh:   make(chan Record, 4096),
		stopCh:    make(chan struct{}),
		maxPerSec: 200,
		secStart:  time.Now(),
	}
	go c.writer()
	return c
}

// Subscribe registers a live stream of records.
func (c *Collector) Subscribe(buffer int) (int, <-chan Record) {
	if buffer <= 0 {
		buffer = 256
	}
	ch := make(chan Record, buffer)
	c.mu.Lock()
	id := c.next
	c.next++
	c.subs[id] = ch
	c.mu.Unlock()
	return id, ch
}

// Unsubscribe removes a subscription and closes its channel.
func (c *Collector) Unsubscribe(id int) {
	c.mu.Lock()
	if ch, ok := c.subs[id]; ok {
		delete(c.subs, id)
		close(ch)
	}
	c.mu.Unlock()
}

// Add records an event.
func (c *Collector) Add(r Record) {
	if r.TS == 0 {
		r.TS = time.Now().UnixMilli()
	}
	c.mu.Lock()
	if len(c.ring) < c.max {
		c.ring = append(c.ring, r)
	} else {
		copy(c.ring, c.ring[len(c.ring)-c.max+1:])
		c.ring[len(c.ring)-1] = r
	}
	subs := make([]chan Record, 0, len(c.subs))
	for _, ch := range c.subs {
		subs = append(subs, ch)
	}
	c.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- r:
		default:
			c.dropped.Add(1)
		}
	}
	if c.st != nil {
		if c.allowPersist() {
			select {
			case c.writeCh <- r:
				c.persisted.Add(1)
			default:
				// Persistence is best-effort; the UI keeps the in-memory view.
			}
		} else {
			c.throttled.Add(1)
		}
	}
}

// Recent returns up to n most recent records (newest first).
func (c *Collector) Recent(n int) []Record {
	if n <= 0 || n > c.max {
		n = c.max
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Record, 0, n)
	for i := len(c.ring) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, c.ring[i])
	}
	return out
}

// Dropped reports how many live events were skipped due to slow subscribers.
func (c *Collector) Dropped() int64 { return c.dropped.Load() }

// allowPersist implements the per-second disk write budget.
func (c *Collector) allowPersist() bool {
	c.secMu.Lock()
	defer c.secMu.Unlock()
	now := time.Now()
	if now.Sub(c.secStart) >= time.Second {
		c.secStart = now
		c.secCount = 0
	}
	if c.secCount >= c.maxPerSec {
		return false
	}
	c.secCount++
	return true
}

// PersistStats exposes how much of the audit stream reached disk.
func (c *Collector) PersistStats() (persisted, throttled int64) {
	return c.persisted.Load(), c.throttled.Load()
}

func (c *Collector) writer() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	batch := make([]Record, 0, 64)
	flush := func() {
		if c.st == nil || len(batch) == 0 {
			batch = batch[:0]
			return
		}
		for _, r := range batch {
			_ = c.st.AddAudit(store.AuditRow{
				TS: r.TS, Module: r.Module, Source: r.Source, Target: r.Target, Action: r.Action,
				NetworkMode: r.NetworkMode, BytesUp: r.BytesUp, BytesDown: r.BytesDown,
				DurationMS: r.DurationMS, Status: r.Status, Error: r.Error, RuleID: r.RuleID,
			})
		}
		batch = batch[:0]
	}
	for {
		select {
		case <-c.stopCh:
			flush()
			return
		case r := <-c.writeCh:
			batch = append(batch, r)
			if len(batch) >= 64 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// Close flushes and stops the persistence worker.
func (c *Collector) Close() {
	c.stopOnce.Do(func() { close(c.stopCh) })
}
