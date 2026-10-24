// Package forward implements port mapping on top of tunnel channels.
//
// One tunnel port can carry an unlimited number of mappings: each rule opens a
// local listener whose accepted sockets are paired with a fresh mux channel
// toward the peer, which then dials the real target. This is the "single port,
// many tunnels" model.
package forward

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LECYWZA/mytun/internal/mux"
)

// Rule is one port mapping.
type Rule struct {
	ID             string `json:"id"`
	Role           string `json:"role"` // "server" or "client": which end listens
	ListenHost     string `json:"listenHost"`
	ListenPort     int    `json:"listenPort"`
	TargetHost     string `json:"targetHost"`
	TargetPort     int    `json:"targetPort"`
	TargetClientID string `json:"targetClientId"`
	Enabled        bool   `json:"enabled"`
}

// Event describes one finished mapping session, used by the audit log.
type Event struct {
	RuleID    string
	Role      string
	Source    string
	Target    string
	BytesUp   int64
	BytesDown int64
	Duration  time.Duration
	Status    string
	Error     string
	StartedAt time.Time
}

// SessionResolver returns the tunnel session for a client id. The tunnel
// layer wires this to the server registry or the client manager.
type SessionResolver func(clientID string) *mux.Session

// Forwarder owns all listeners for one role.
type Forwarder struct {
	role     string
	resolve  SessionResolver
	onEvent  func(Event)

	mu       sync.Mutex
	listeners map[int]*listener
	rules     map[int]Rule
	// sem caps concurrent bridged connections.
	sem chan struct{}
}

type listener struct {
	rule   Rule
	ln     net.Listener
	closed atomic.Bool
}

// New creates a forwarder for the given role ("server"/"client").
func New(role string, resolve SessionResolver, onEvent func(Event)) *Forwarder {
	return &Forwarder{
		role:      role,
		resolve:   resolve,
		onEvent:   onEvent,
		listeners: map[int]*listener{},
		rules:     map[int]Rule{},
		sem:       make(chan struct{}, 2048),
	}
}

// Apply syncs listeners to the desired rule set. Removed or disabled rules
// have their listeners closed; new ones are opened. Errors are returned per
// rule without aborting the rest.
func (f *Forwarder) Apply(rules []Rule) map[int]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	want := map[int]Rule{}
	dupes := map[int]string{}
	for _, r := range rules {
		if r.Role != "" && r.Role != f.role {
			continue
		}
		if !r.Enabled || r.ListenPort <= 0 {
			continue
		}
		if _, clash := want[r.ListenPort]; clash {
			// Two rules on one port: keep the first and surface the clash
			// instead of silently dropping one of them.
			dupes[r.ListenPort] = fmt.Sprintf("port %d is used by more than one forward rule", r.ListenPort)
			continue
		}
		want[r.ListenPort] = r
	}

	errs := map[int]string{}
	for port, msg := range dupes {
		errs[port] = msg
	}
	for port, l := range f.listeners {
		if _, ok := want[port]; !ok {
			l.closed.Store(true)
			_ = l.ln.Close()
			delete(f.listeners, port)
			delete(f.rules, port)
		}
	}
	for port, r := range want {
		if l, ok := f.listeners[port]; ok {
			// Rule content may have changed (target host/port). Recreate so the
			// new target takes effect immediately.
			if l.rule.TargetHost != r.TargetHost || l.rule.TargetPort != r.TargetPort || l.rule.TargetClientID != r.TargetClientID || l.rule.ListenHost != r.ListenHost {
				l.closed.Store(true)
				_ = l.ln.Close()
				delete(f.listeners, port)
			} else {
				continue
			}
		}
		ln, err := f.listen(r)
		if err != nil {
			errs[port] = err.Error()
			continue
		}
		f.listeners[port] = ln
		f.rules[port] = r
	}
	return errs
}

// Stop closes every listener.
func (f *Forwarder) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for port, l := range f.listeners {
		l.closed.Store(true)
		_ = l.ln.Close()
		delete(f.listeners, port)
	}
	f.rules = map[int]Rule{}
}

// Listening reports the ports currently bound.
func (f *Forwarder) Listening() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, 0, len(f.listeners))
	for p := range f.listeners {
		out = append(out, p)
	}
	return out
}

func (f *Forwarder) listen(r Rule) (*listener, error) {
	host := r.ListenHost
	if host == "" {
		host = "0.0.0.0"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(r.ListenPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	l := &listener{rule: r, ln: ln}
	go f.acceptLoop(l)
	return l, nil
}

func (f *Forwarder) acceptLoop(l *listener) {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if l.closed.Load() {
				return
			}
			select {
			case <-time.After(30 * time.Millisecond):
				continue
			}
		}
		select {
		case f.sem <- struct{}{}:
			go func() {
				defer func() { <-f.sem }()
				f.handleLocalConn(l.rule, conn)
			}()
		default:
			_ = conn.Close()
		}
	}
}

// handleLocalConn bridges an inbound TCP connection to the peer over a channel.
func (f *Forwarder) handleLocalConn(rule Rule, conn net.Conn) {
	start := time.Now()
	ev := Event{RuleID: rule.ID, Role: f.role, Source: conn.RemoteAddr().String(),
		Target: net.JoinHostPort(rule.TargetHost, strconv.Itoa(rule.TargetPort)), Status: "success", StartedAt: start}
	defer func() {
		_ = conn.Close()
		ev.Duration = time.Since(start)
		if f.onEvent != nil {
			f.onEvent(ev)
		}
	}()

	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
		_ = tcp.SetKeepAlive(true)
	}

	sess := f.resolve(rule.TargetClientID)
	if sess == nil {
		ev.Status = "failed"
		ev.Error = "no tunnel session for target client"
		return
	}

	ch, err := sess.OpenChannel(map[string]any{
		"type": mux.MetaForward,
		"host": rule.TargetHost,
		"port": rule.TargetPort,
		"rule": rule.ID,
	})
	if err != nil {
		ev.Status = "failed"
		ev.Error = err.Error()
		return
	}
	ok, err := ch.AwaitAck(8 * time.Second)
	if err != nil || !ok {
		_ = ch.Close()
		ev.Status = "failed"
		if err != nil {
			ev.Error = err.Error()
		} else {
			ev.Error = "remote target unreachable: " + ev.Target
		}
		return
	}
	up, down := bridge(conn, ch)
	ev.BytesUp = up
	ev.BytesDown = down
}

// HandleChannel serves a channel opened by the peer: dial the requested target
// and bridge it. Used by both roles (full duplex tunnels).
func (f *Forwarder) HandleChannel(sess *mux.Session, ch *mux.Channel) bool {
	typ, _ := ch.Meta["type"].(string)
	if typ != mux.MetaForward && typ != "tcp" {
		return false
	}
	host, _ := ch.Meta["host"].(string)
	port := 0
	switch v := ch.Meta["port"].(type) {
	case float64:
		port = int(v)
	case int:
		port = v
	case string:
		port, _ = strconv.Atoi(v)
	}
	if host == "" || port == 0 {
		sess.SendACK(ch.ID(), false)
		_ = ch.Close()
		return true
	}
	go f.dialAndBridge(sess, ch, net.JoinHostPort(host, strconv.Itoa(port)))
	return true
}

func (f *Forwarder) dialAndBridge(sess *mux.Session, ch *mux.Channel, addr string) {
	start := time.Now()
	ruleID, _ := ch.Meta["rule"].(string)
	ev := Event{RuleID: ruleID, Role: f.role, Target: addr, Status: "success", StartedAt: start}
	defer func() {
		ev.Duration = time.Since(start)
		if f.onEvent != nil {
			f.onEvent(ev)
		}
	}()

	d := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		ev.Status = "failed"
		ev.Error = err.Error()
		sess.SendACK(ch.ID(), false)
		_ = ch.Close()
		return
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
		_ = tcp.SetKeepAlive(true)
	}
	ev.Source = conn.LocalAddr().String()
	sess.SendACK(ch.ID(), true)
	up, down := bridge(conn, ch)
	ev.BytesUp = down
	ev.BytesDown = up
	_ = conn.Close()
	_ = ch.Close()
}

// bridge copies both directions and returns (bytes from a->b, bytes from b->a).
func bridge(conn net.Conn, ch *mux.Channel) (int64, int64) {
	var up, down atomic.Int64
	done := make(chan struct{}, 2)

	go func() {
		n, _ := io.Copy(writerFunc(func(p []byte) (int, error) { return ch.Write(p) }), conn)
		up.Store(n)
		_ = ch.Close()
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(conn, ch)
		down.Store(n)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = conn.Close()
		}
		done <- struct{}{}
	}()
	<-done
	// Give the other direction a bounded window to drain. Unbounded waiting here
	// leaks a socket per aborted client, which shows up as fd growth on a busy
	// tunnel.
	select {
	case <-done:
	case <-time.After(20 * time.Second):
	}
	// Close both ends so the remaining copy() returns and its fd is released.
	_ = conn.Close()
	_ = ch.Close()
	return up.Load(), down.Load()
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// ErrNoSession is returned when a rule has no usable tunnel.
var ErrNoSession = errors.New("forward: no tunnel session")

// Describe renders a rule target for logs.
func (r Rule) Describe() string {
	return fmt.Sprintf("%s:%d -> %s:%d", r.ListenHost, r.ListenPort, r.TargetHost, r.TargetPort)
}
