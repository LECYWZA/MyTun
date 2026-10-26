package forward

import (
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// LocalRule is a plain port forward that never touches a tunnel: it listens on
// this machine and dials the target directly, like nginx's stream{} block.
//
// Network selects the protocol: "tcp" (default), "udp", or "both".
type LocalRule struct {
	ID         string
	Network    string
	ListenHost string
	ListenPort int
	TargetHost string
	TargetPort int
	Enabled    bool
}

// LocalForwarder owns the plain TCP/UDP listeners for local rules.
type LocalForwarder struct {
	mu        sync.Mutex
	listeners map[int]*localListener
	onEvent   func(Event)
	running   atomic.Bool
	conns     atomic.Int64
}

type localListener struct {
	rule   LocalRule
	tcp    net.Listener
	udp    net.PacketConn
	closed atomic.Bool
}

// NewLocal builds a local forwarder. onEvent may be nil.
func NewLocal(onEvent func(Event)) *LocalForwarder {
	l := &LocalForwarder{listeners: map[int]*localListener{}, onEvent: onEvent}
	l.running.Store(true)
	return l
}

// Conns reports the number of live relayed connections.
func (l *LocalForwarder) Conns() int64 { return l.conns.Load() }

// Ports lists ports that currently have a listener.
func (l *LocalForwarder) Ports() []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]int, 0, len(l.listeners))
	for p := range l.listeners {
		out = append(out, p)
	}
	return out
}

// Apply reconciles the running listeners with the desired rule set, returning
// per-port errors so the console can show which rule failed to bind.
func (l *LocalForwarder) Apply(rules []LocalRule) map[int]string {
	l.mu.Lock()
	defer l.mu.Unlock()

	errs := map[int]string{}
	want := map[int]LocalRule{}
	for _, r := range rules {
		if !r.Enabled || r.ListenPort <= 0 || r.TargetPort <= 0 {
			continue
		}
		if _, clash := want[r.ListenPort]; clash {
			errs[r.ListenPort] = "port " + strconv.Itoa(r.ListenPort) + " is used by more than one forward rule"
			continue
		}
		want[r.ListenPort] = r
	}

	for port, lis := range l.listeners {
		r, keep := want[port]
		if !keep || !sameLocalRule(lis.rule, r) {
			lis.close()
			delete(l.listeners, port)
		}
	}

	for port, r := range want {
		if _, ok := l.listeners[port]; ok {
			continue
		}
		lis := &localListener{rule: r}
		if err := lis.open(l); err != nil {
			errs[port] = err.Error()
			continue
		}
		l.listeners[port] = lis
	}
	return errs
}

// Stop closes every listener.
func (l *LocalForwarder) Stop() {
	l.running.Store(false)
	l.mu.Lock()
	defer l.mu.Unlock()
	for port, lis := range l.listeners {
		lis.close()
		delete(l.listeners, port)
	}
}

func sameLocalRule(a, b LocalRule) bool {
	return a.ID == b.ID && a.Network == b.Network && a.ListenHost == b.ListenHost &&
		a.ListenPort == b.ListenPort && a.TargetHost == b.TargetHost && a.TargetPort == b.TargetPort
}

func (l *LocalForwarder) emit(ev Event) {
	if l.onEvent != nil {
		l.onEvent(ev)
	}
}

func (lis *localListener) close() {
	lis.closed.Store(true)
	if lis.tcp != nil {
		_ = lis.tcp.Close()
	}
	if lis.udp != nil {
		_ = lis.udp.Close()
	}
}

func (lis *localListener) open(l *LocalForwarder) error {
	r := lis.rule
	host := r.ListenHost
	if host == "" {
		host = "0.0.0.0"
	}
	target := net.JoinHostPort(r.TargetHost, strconv.Itoa(r.TargetPort))
	mode := r.Network
	if mode == "" {
		mode = "tcp"
	}

	if mode == "tcp" || mode == "both" {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(r.ListenPort)))
		if err != nil {
			return err
		}
		lis.tcp = ln
		go lis.serveTCP(l, ln, target)
	}
	if mode == "udp" || mode == "both" {
		pc, err := net.ListenPacket("udp", net.JoinHostPort(host, strconv.Itoa(r.ListenPort)))
		if err != nil {
			if lis.tcp != nil {
				_ = lis.tcp.Close()
				lis.tcp = nil
			}
			return err
		}
		lis.udp = pc
		go lis.serveUDP(l, pc, target)
	}
	return nil
}

func (lis *localListener) serveTCP(l *LocalForwarder, ln net.Listener, target string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if lis.closed.Load() {
				return
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		go lis.relayTCP(l, conn, target)
	}
}

func (lis *localListener) relayTCP(l *LocalForwarder, src net.Conn, target string) {
	started := time.Now()
	ev := Event{
		RuleID: lis.rule.ID, Role: "local", Source: src.RemoteAddr().String(),
		Target: target, StartedAt: started,
	}
	defer func() {
		ev.Duration = time.Since(started)
		l.emit(ev)
	}()

	if tcp, ok := src.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
		_ = tcp.SetKeepAlive(true)
	}
	dst, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		ev.Status = "failed"
		ev.Error = err.Error()
		_ = src.Close()
		return
	}
	l.conns.Add(1)
	defer l.conns.Add(-1)

	var up, down atomic.Int64
	done := make(chan struct{}, 2)
	go func() { n, _ := io.Copy(dst, src); up.Store(n); done <- struct{}{} }()
	go func() { n, _ := io.Copy(src, dst); down.Store(n); done <- struct{}{} }()
	<-done
	_ = src.Close()
	_ = dst.Close()
	<-done
	ev.BytesUp, ev.BytesDown, ev.Status = up.Load(), down.Load(), "success"
}

// udpSession tracks one client address talking through the listener.
type udpSession struct {
	upstream net.Conn
	last     atomic.Int64
}

func (lis *localListener) serveUDP(l *LocalForwarder, pc net.PacketConn, target string) {
	sessions := sync.Map{}
	buf := make([]byte, 64*1024)

	// Reap idle sessions so a dead client does not pin a socket forever.
	go func() {
		for !lis.closed.Load() {
			time.Sleep(15 * time.Second)
			sessions.Range(func(k, v any) bool {
				us := v.(*udpSession)
				if time.Since(time.Unix(0, us.last.Load())) > 60*time.Second {
					_ = us.upstream.Close()
					sessions.Delete(k)
				}
				return true
			})
		}
	}()

	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if lis.closed.Load() {
				return
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		key := addr.String()
		var us *udpSession
		if v, ok := sessions.Load(key); ok {
			us = v.(*udpSession)
		} else {
			up, derr := net.DialTimeout("udp", target, 10*time.Second)
			if derr != nil {
				l.emit(Event{
					RuleID: lis.rule.ID, Role: "local", Source: key, Target: target,
					Status: "failed", Error: derr.Error(), StartedAt: time.Now(),
				})
				continue
			}
			us = &udpSession{upstream: up}
			sessions.Store(key, us)
			go lis.pumpUDP(l, pc, us, addr, target)
		}
		us.last.Store(time.Now().UnixNano())
		_, _ = us.upstream.Write(buf[:n])
	}
}

// pumpUDP relays replies from the target back to the client address.
func (lis *localListener) pumpUDP(l *LocalForwarder, pc net.PacketConn, us *udpSession, addr net.Addr, target string) {
	started := time.Now()
	ev := Event{RuleID: lis.rule.ID, Role: "local", Source: addr.String(), Target: target, StartedAt: started}
	var up, down int64
	buf := make([]byte, 64*1024)
	l.conns.Add(1)
	defer l.conns.Add(-1)
	for {
		_ = us.upstream.SetReadDeadline(time.Now().Add(60 * time.Second))
		n, err := us.upstream.Read(buf)
		if err != nil {
			ev.Duration = time.Since(started)
			ev.BytesUp, ev.BytesDown, ev.Status = up, down, "success"
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				ev.Status = "idle"
			}
			l.emit(ev)
			return
		}
		down += int64(n)
		_, _ = pc.WriteTo(buf[:n], addr)
	}
}
