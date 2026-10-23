// Package proxy implements a mixed HTTP/SOCKS5 proxy port.
//
// Protocol sniffing mirrors Bi-Tunnel: the first byte decides SOCKS5 (0x05) vs
// HTTP. Egress is intentionally limited to two modes:
//
//	local  -> dial the target directly from this machine
//	remote -> open a tunnel channel to the peer, which dials the target
//
// No proxy pools, chains or rule engines: that complexity is gone by design.
package proxy

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LECYWZA/mytun/internal/mux"
)

// Rule configures one mixed proxy listener.
type Rule struct {
	ID             string   `json:"id"`
	Role           string   `json:"role"`
	ListenHost     string   `json:"listenHost"`
	ListenPort     int      `json:"listenPort"`
	NetworkMode    string   `json:"networkMode"` // local | remote
	TargetClientID string   `json:"targetClientId"`
	UseAuth        bool     `json:"useAuth"`
	User           string   `json:"user"`
	Pass           string   `json:"pass"`
	AllowIPs       []string `json:"allowIps"`
	DenyIPs        []string `json:"denyIps"`
	Enabled        bool     `json:"enabled"`
}

// Event is one proxied connection for the audit log.
type Event struct {
	RuleID    string
	Module    string
	Source    string
	Target    string
	Action    string
	NetworkMode string
	BytesUp   int64
	BytesDown int64
	Duration  time.Duration
	Status    string
	Error     string
	StartedAt time.Time
}

// Resolver returns the tunnel session for a client id.
type Resolver func(clientID string) *mux.Session

// Proxy owns all mixed-proxy listeners.
type Proxy struct {
	role    string
	resolve Resolver
	onEvent func(Event)
	// LocalBypass reports whether localhost clients may skip auth (true while
	// the system proxy is enabled, so desktop apps do not need credentials).
	LocalBypass func() bool

	mu        sync.Mutex
	listeners map[int]*proxyListener
	rules     map[int]Rule
	totalConn atomic.Int64
	// sem caps concurrent proxied connections so a burst cannot exhaust fds.
	sem chan struct{}
}

type proxyListener struct {
	rule   Rule
	ln     net.Listener
	closed atomic.Bool
}

// New creates a proxy manager.
func New(role string, resolve Resolver, onEvent func(Event)) *Proxy {
	return &Proxy{role: role, resolve: resolve, onEvent: onEvent,
		listeners: map[int]*proxyListener{}, rules: map[int]Rule{}, sem: make(chan struct{}, 2048)}
}

// Apply syncs listeners with the desired rules.
func (p *Proxy) Apply(rules []Rule) map[int]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	want := map[int]Rule{}
	for _, r := range rules {
		if r.Role != "" && r.Role != p.role {
			continue
		}
		if !r.Enabled || r.ListenPort <= 0 {
			continue
		}
		want[r.ListenPort] = r
	}
	errs := map[int]string{}
	for port, l := range p.listeners {
		if _, ok := want[port]; !ok {
			l.closed.Store(true)
			_ = l.ln.Close()
			delete(p.listeners, port)
			delete(p.rules, port)
		}
	}
	for port, r := range want {
		if l, ok := p.listeners[port]; ok {
			if l.rule.ListenHost != r.ListenHost || l.rule.NetworkMode != r.NetworkMode ||
				l.rule.TargetClientID != r.TargetClientID || l.rule.UseAuth != r.UseAuth ||
				l.rule.User != r.User || l.rule.Pass != r.Pass {
				l.closed.Store(true)
				_ = l.ln.Close()
				delete(p.listeners, port)
			} else {
				continue
			}
		}
		host := r.ListenHost
		if host == "" {
			host = "0.0.0.0"
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(r.ListenPort)))
		if err != nil {
			errs[port] = err.Error()
			continue
		}
		l := &proxyListener{rule: r, ln: ln}
		p.listeners[port] = l
		p.rules[port] = r
		go p.acceptLoop(l)
	}
	return errs
}

// Stop closes all listeners.
func (p *Proxy) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for port, l := range p.listeners {
		l.closed.Store(true)
		_ = l.ln.Close()
		delete(p.listeners, port)
	}
	p.rules = map[int]Rule{}
}

// Listening returns bound ports.
func (p *Proxy) Listening() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]int, 0, len(p.listeners))
	for port := range p.listeners {
		out = append(out, port)
	}
	return out
}

// ActiveConnections reports accepted proxied connections since start.
func (p *Proxy) ActiveConnections() int64 { return p.totalConn.Load() }

func (p *Proxy) acceptLoop(l *proxyListener) {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if l.closed.Load() {
				return
			}
			time.Sleep(30 * time.Millisecond)
			continue
		}
		p.totalConn.Add(1)
		select {
		case p.sem <- struct{}{}:
			go func() {
				defer func() { <-p.sem }()
				p.handle(conn, l.rule)
			}()
		default:
			// Over the cap: drop immediately rather than queueing forever.
			_ = conn.Close()
		}
	}
}

func (p *Proxy) handle(conn net.Conn, rule Rule) {
	start := time.Now()
	src := conn.RemoteAddr().String()
	ev := Event{RuleID: rule.ID, Module: "proxy", Source: src, NetworkMode: rule.NetworkMode,
		Status: "success", StartedAt: start}
	defer func() {
		_ = conn.Close()
		ev.Duration = time.Since(start)
		if p.onEvent != nil {
			p.onEvent(ev)
		}
	}()

	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
		_ = tcp.SetKeepAlive(true)
	}
	if !ipAllowed(conn.RemoteAddr(), rule.AllowIPs, rule.DenyIPs) {
		ev.Status = "blocked"
		ev.Error = "source ACL denied"
		return
	}

	br := bufio.NewReaderSize(conn, 32*1024)
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	first, err := br.Peek(1)
	if err != nil {
		ev.Status = "failed"
		ev.Error = err.Error()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	if first[0] == 0x05 {
		ev.Module = "socks5"
		p.handleSOCKS5(conn, br, rule, &ev)
	} else {
		ev.Module = "http"
		p.handleHTTP(conn, br, rule, &ev)
	}
}

// ---- SOCKS5 ----

func (p *Proxy) handleSOCKS5(conn net.Conn, br *bufio.Reader, rule Rule, ev *Event) {
	requireAuth := rule.UseAuth && !p.bypassAuth(conn)

	// greeting: VER NMETHODS METHODS
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil {
		ev.Status = "failed"
		ev.Error = "socks5 greeting: " + err.Error()
		return
	}
	if hdr[0] != 0x05 {
		ev.Status = "failed"
		ev.Error = "socks5: bad version"
		return
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(br, methods); err != nil {
		ev.Status = "failed"
		ev.Error = err.Error()
		return
	}
	if requireAuth {
		if !hasMethod(methods, 0x02) {
			_, _ = conn.Write([]byte{0x05, 0xFF})
			ev.Status = "failed"
			ev.Error = "socks5: client does not support username/password"
			return
		}
		if _, err := conn.Write([]byte{0x05, 0x02}); err != nil {
			return
		}
		if !p.socks5Auth(conn, rule, ev) {
			return
		}
	} else {
		if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
			return
		}
	}

	host, port, err := readSOCKS5Request(br)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		ev.Status = "failed"
		ev.Error = err.Error()
		return
	}
	ev.Target = net.JoinHostPort(host, strconv.Itoa(port))
	ev.Action = rule.NetworkMode

	peer, err := p.openEgress(rule, host, port)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		ev.Status = "failed"
		ev.Error = err.Error()
		return
	}
	defer peer.Close()

	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	up, down := pipeBoth(conn, br, peer)
	ev.BytesUp, ev.BytesDown = up, down
}

func (p *Proxy) socks5Auth(conn net.Conn, rule Rule, ev *Event) bool {
	// sub-negotiation: VER ULEN UNAME PLEN PASSWD
	h := make([]byte, 2)
	if _, err := io.ReadFull(conn, h); err != nil {
		ev.Status = "failed"
		ev.Error = err.Error()
		return false
	}
	if h[0] != 0x01 {
		_, _ = conn.Write([]byte{0x01, 0x01})
		ev.Status = "failed"
		ev.Error = "socks5: bad auth version"
		return false
	}
	ubuf := make([]byte, int(h[1]))
	if _, err := io.ReadFull(conn, ubuf); err != nil {
		ev.Status = "failed"
		ev.Error = err.Error()
		return false
	}
	pl := make([]byte, 1)
	if _, err := io.ReadFull(conn, pl); err != nil {
		ev.Status = "failed"
		ev.Error = err.Error()
		return false
	}
	pbuf := make([]byte, int(pl[0]))
	if _, err := io.ReadFull(conn, pbuf); err != nil {
		ev.Status = "failed"
		ev.Error = err.Error()
		return false
	}
	if string(ubuf) == rule.User && string(pbuf) == rule.Pass {
		_, _ = conn.Write([]byte{0x01, 0x00})
		return true
	}
	_, _ = conn.Write([]byte{0x01, 0x01})
	ev.Status = "failed"
	ev.Error = "socks5: auth failed"
	return false
}

func readSOCKS5Request(br *bufio.Reader) (string, int, error) {
	h := make([]byte, 4)
	if _, err := io.ReadFull(br, h); err != nil {
		return "", 0, err
	}
	if h[0] != 0x05 {
		return "", 0, errors.New("socks5: bad version")
	}
	if h[1] != 0x01 {
		return "", 0, fmt.Errorf("socks5: unsupported command %d", h[1])
	}
	var host string
	switch h[3] {
	case 0x01:
		b := make([]byte, 4)
		if _, err := io.ReadFull(br, b); err != nil {
			return "", 0, err
		}
		host = net.IP(b).String()
	case 0x03:
		l, err := br.ReadByte()
		if err != nil {
			return "", 0, err
		}
		b := make([]byte, int(l))
		if _, err := io.ReadFull(br, b); err != nil {
			return "", 0, err
		}
		host = string(b)
	case 0x04:
		b := make([]byte, 16)
		if _, err := io.ReadFull(br, b); err != nil {
			return "", 0, err
		}
		host = net.IP(b).String()
	default:
		return "", 0, fmt.Errorf("socks5: unsupported address type %d", h[3])
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(br, pb); err != nil {
		return "", 0, err
	}
	return host, int(pb[0])<<8 | int(pb[1]), nil
}

func hasMethod(methods []byte, want byte) bool {
	for _, m := range methods {
		if m == want {
			return true
		}
	}
	return false
}

// ---- HTTP ----

func (p *Proxy) handleHTTP(conn net.Conn, br *bufio.Reader, rule Rule, ev *Event) {
	requireAuth := rule.UseAuth && !p.bypassAuth(conn)

	var sb strings.Builder
	// Read headers line by line with a hard cap to avoid unbounded buffering.
	var total int
	var firstLine string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			ev.Status = "failed"
			ev.Error = "http: read headers: " + err.Error()
			return
		}
		total += len(line)
		if total > 64*1024 {
			_, _ = conn.Write([]byte("HTTP/1.1 431 Request Header Fields Too Large\r\n\r\n"))
			ev.Status = "failed"
			ev.Error = "http: headers too large"
			return
		}
		if firstLine == "" {
			firstLine = strings.TrimRight(line, "\r\n")
		}
		sb.WriteString(line)
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	fields := strings.Fields(firstLine)
	if len(fields) < 2 {
		_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		ev.Status = "failed"
		ev.Error = "http: malformed request line"
		return
	}
	method := fields[0]
	target := fields[1]

	if requireAuth {
		auth := extractHeader(sb.String(), "proxy-authorization")
		if auth == "" {
			_, _ = conn.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"MyTun\"\r\n\r\n"))
			ev.Status = "failed"
			ev.Error = "http: 407 auth required"
			return
		}
		if !strings.HasPrefix(strings.ToLower(auth), "basic ") {
			_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\n\r\n"))
			ev.Status = "failed"
			ev.Error = "http: unsupported auth scheme"
			return
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(auth[6:]))
		if err != nil {
			_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\n\r\n"))
			ev.Status = "failed"
			ev.Error = "http: bad base64 credentials"
			return
		}
		u, pw, _ := strings.Cut(string(raw), ":")
		if u != rule.User || pw != rule.Pass {
			_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\n\r\n"))
			ev.Status = "failed"
			ev.Error = "http: bad credentials"
			return
		}
	}

	host, port := "", 0
	switch {
	case method == "CONNECT":
		h, pt, err := net.SplitHostPort(target)
		if err != nil {
			_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
			ev.Status = "failed"
			ev.Error = "http: bad CONNECT target " + target
			return
		}
		host, _ = h, 0
		port, _ = strconv.Atoi(pt)
	case strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://"):
		u := target
		if i := strings.Index(u, "://"); i >= 0 {
			u = u[i+3:]
		}
		if i := strings.IndexByte(u, '/'); i >= 0 {
			u = u[:i]
		}
		h, pt, err := net.SplitHostPort(u)
		if err != nil {
			host = u
			if strings.HasPrefix(target, "https://") {
				port = 443
			} else {
				port = 80
			}
		} else {
			host = h
			port, _ = strconv.Atoi(pt)
		}
	default:
		hdrHost := extractHeader(sb.String(), "host")
		if hdrHost == "" {
			_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
			ev.Status = "failed"
			ev.Error = "http: missing Host header"
			return
		}
		host, port = hdrHost, 80
		if h, pt, err := net.SplitHostPort(hdrHost); err == nil {
			host, _ = h, 0
			port, _ = strconv.Atoi(pt)
		}
	}
	if port == 0 {
		port = 80
	}
	if strings.HasPrefix(target, "https://") && port == 0 {
		port = 443
	}
	ev.Target = net.JoinHostPort(host, strconv.Itoa(port))
	ev.Action = rule.NetworkMode

	peer, err := p.openEgress(rule, host, port)
	if err != nil {
		_, _ = conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		ev.Status = "failed"
		ev.Error = err.Error()
		return
	}
	defer peer.Close()

	if method == "CONNECT" {
		if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			return
		}
		up, down := pipeBoth(conn, br, peer)
		ev.BytesUp, ev.BytesDown = up, down
		return
	}
	// Plain HTTP: rewrite the request line to origin-form (forward proxies must
	// not leak the absolute URI to the origin server) and drop hop-by-hop proxy
	// headers before forwarding.
	outHeaders := rewriteRequestHeaders(sb.String(), target)
	if _, err := peer.Write([]byte(outHeaders)); err != nil {
		ev.Status = "failed"
		ev.Error = err.Error()
		return
	}
	up, down := pipeBoth(conn, br, peer)
	ev.BytesUp, ev.BytesDown = up, down
}

func (p *Proxy) bypassAuth(conn net.Conn) bool {
	if p.LocalBypass == nil || !p.LocalBypass() {
		return false
	}
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// openEgress dials the target either locally or through the tunnel.
func (p *Proxy) openEgress(rule Rule, host string, port int) (net.Conn, error) {
	if rule.NetworkMode == "remote" {
		if p.resolve == nil {
			return nil, errors.New("no tunnel resolver")
		}
		sess := p.resolve(rule.TargetClientID)
		if sess == nil {
			return nil, errors.New("tunnel not connected")
		}
		ch, err := sess.OpenChannel(map[string]any{
			"type": mux.MetaForward,
			"host": host,
			"port": port,
			"via":  "proxy",
		})
		if err != nil {
			return nil, err
		}
		ok, err := ch.AwaitAck(8 * time.Second)
		if err != nil {
			_ = ch.Close()
			return nil, err
		}
		if !ok {
			_ = ch.Close()
			return nil, fmt.Errorf("remote dial failed: %s:%d", host, port)
		}
		return ch, nil
	}
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := d.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	return conn, nil
}

func pipeBoth(conn net.Conn, br *bufio.Reader, peer net.Conn) (int64, int64) {
	var up, down atomic.Int64
	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(peer, br)
		up.Store(n)
		if cw, ok := peer.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(conn, peer)
		down.Store(n)
		done <- struct{}{}
	}()
	// Standard proxy teardown: the FIRST direction to finish ends the exchange.
	// Waiting for both strands forever hangs on keep-alive clients (the request
	// reader never sees EOF until the socket closes), which is exactly the bug
	// that made a second curl call stall.
	<-done
	_ = peer.Close()
	_ = conn.Close()
	<-done
	return up.Load(), down.Load()
}

func extractHeader(raw, name string) string {
	name = strings.ToLower(name) + ":"
	for _, line := range strings.Split(raw, "\n") {
		l := strings.TrimRight(line, "\r")
		if len(l) >= len(name) && strings.EqualFold(l[:len(name)], name) {
			return strings.TrimSpace(l[len(name):])
		}
	}
	return ""
}

// ipAllowed evaluates source IP ACLs (CIDR or plain IP).
func ipAllowed(addr net.Addr, allow, deny []string) bool {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	match := func(list []string) bool {
		for _, item := range list {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if strings.Contains(item, "/") {
				if _, cidr, err := net.ParseCIDR(item); err == nil && cidr.Contains(ip) {
					return true
				}
				continue
			}
			if p := net.ParseIP(item); p != nil && p.Equal(ip) {
				return true
			}
		}
		return false
	}
	if len(deny) > 0 && match(deny) {
		return false
	}
	if len(allow) > 0 {
		return match(allow)
	}
	return true
}

// rewriteRequestHeaders converts an absolute-URI request line into origin-form
// and strips proxy-only headers.
func rewriteRequestHeaders(raw, absoluteTarget string) string {
	lines := strings.Split(raw, "\n")
	if len(lines) == 0 {
		return raw
	}
	first := strings.TrimRight(lines[0], "\r")
	parts := strings.Fields(first)
	if len(parts) >= 2 && strings.HasPrefix(parts[1], "http://") {
		rest := parts[1][len("http://"):]
		path := "/"
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			path = rest[i:]
		}
		lines[0] = parts[0] + " " + path + " " + strings.Join(parts[2:], " ")
		if len(parts) < 3 {
			lines[0] = parts[0] + " " + path + " HTTP/1.1"
		}
	}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		l := strings.TrimRight(line, "\r")
		low := strings.ToLower(l)
		if strings.HasPrefix(low, "proxy-authorization:") || strings.HasPrefix(low, "proxy-connection:") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\r\n")
}
