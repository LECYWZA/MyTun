package tunnel

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LECYWZA/mytun/internal/mux"
)

// ClientConnConfig describes one upstream tunnel endpoint the client dials.
type ClientConnConfig struct {
	ID         string
	Alias      string
	TunnelHost string
	TunnelPort int
	Password   string
	ClientID   string
	SNI        string
	// Limiter shapes outbound frames; nil disables shaping.
	Limiter mux.Limiter
}

// ConnStatus enumerates client link states.
const (
	StatusStopped    = "stopped"
	StatusConnecting = "connecting"
	StatusConnected  = "connected"
	StatusFailed     = "failed"
)

// ClientConn is one supervised upstream link with automatic reconnect.
type ClientConn struct {
	cfg ClientConnConfig

	mu      sync.RWMutex
	session *mux.Session
	status  string
	lastErr string
	retryAt time.Time
	attempts int

	stopCh   chan struct{}
	stopOnce sync.Once

	// OnSession fires on successful authentication.
	OnSession func(*mux.Session)
	// OnSessionClosed fires when an established session drops.
	OnSessionClosed func()
	// OnChannel handles channels created by the server side.
	OnChannel func(*mux.Session, *mux.Channel)
	// OnStatus reports status transitions to the UI.
	OnStatus func(id, status, msg string)

	sealer *mux.Sealer
}

// NewClientConn builds a supervised link.
func NewClientConn(cfg ClientConnConfig) (*ClientConn, error) {
	if strings.TrimSpace(cfg.Password) == "" {
		return nil, errors.New("client: password is required")
	}
	if cfg.TunnelHost == "" || cfg.TunnelPort == 0 {
		return nil, errors.New("client: host and port are required")
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "client-1"
	}
	if cfg.ID == "" {
		cfg.ID = cfg.TunnelHost + ":" + fmt.Sprint(cfg.TunnelPort)
	}
	sealer, err := mux.NewSealer(mux.DeriveKey(cfg.Password))
	if err != nil {
		return nil, err
	}
	return &ClientConn{
		cfg:    cfg,
		sealer: sealer,
		status: StatusStopped,
		stopCh: make(chan struct{}),
	}, nil
}

// Start launches the supervision loop.
func (c *ClientConn) Start() {
	go c.loop()
}

// Stop terminates the link and prevents reconnects.
func (c *ClientConn) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
	c.mu.Lock()
	if c.session != nil {
		_ = c.session.Close()
		c.session = nil
	}
	c.status = StatusStopped
	c.mu.Unlock()
	c.setStatus(StatusStopped, "stopped")
}

// Session returns the live session if authenticated.
func (c *ClientConn) Session() *mux.Session {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.session != nil && c.session.IsAuthed() {
		return c.session
	}
	return nil
}

// State returns a snapshot for the UI.
func (c *ClientConn) State() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	st := map[string]any{
		"id":         c.cfg.ID,
		"alias":      c.cfg.Alias,
		"host":       c.cfg.TunnelHost,
		"port":       c.cfg.TunnelPort,
		"clientId":   c.cfg.ClientID,
		"status":     c.status,
		"lastError":  c.lastErr,
		"attempts":   c.attempts,
	}
	if !c.retryAt.IsZero() {
		st["retryAt"] = c.retryAt
	}
	if c.session != nil {
		st["remoteAddr"] = c.session.RemoteAddr
		st["connectedAt"] = c.session.ConnectedAt
		st["channels"] = c.session.ChannelCount()
	}
	return st
}

// Config exposes the immutable configuration.
func (c *ClientConn) Config() ClientConnConfig { return c.cfg }

func (c *ClientConn) setStatus(status, msg string) {
	c.mu.Lock()
	c.status = status
	if msg != "" {
		c.lastErr = msg
	}
	c.mu.Unlock()
	if c.OnStatus != nil {
		c.OnStatus(c.cfg.ID, status, msg)
	}
}

func (c *ClientConn) loop() {
	backoff := 3 * time.Second
	for {
		select {
		case <-c.stopCh:
			return
		default:
		}
		if err := c.dialAndServe(); err != nil {
			c.mu.Lock()
			c.attempts++
			c.lastErr = err.Error()
			c.retryAt = time.Now().Add(backoff)
			c.mu.Unlock()
			c.setStatus(StatusFailed, err.Error())
		}
		select {
		case <-c.stopCh:
			return
		case <-time.After(backoff):
		}
		// Gentle ramp: 3s, then 3s. Kept deliberately aggressive because the
		// peer may be a VPS that just rebooted.
		backoff = 3 * time.Second
	}
}

func (c *ClientConn) dialAndServe() error {
	c.setStatus(StatusConnecting, "")
	sni := c.cfg.SNI
	if sni == "" {
		sni = "mail.qq.com"
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	raw, err := dialer.Dial("tcp", net.JoinHostPort(c.cfg.TunnelHost, fmt.Sprint(c.cfg.TunnelPort)))
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	if tcp, ok := raw.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	tlsConn := tls.Client(raw, &tls.Config{
		InsecureSkipVerify: true, // self-signed certificates by design
		ServerName:         sni,
		MinVersion:         tls.VersionTLS12,
	})
	_ = tlsConn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		_ = raw.Close()
		return fmt.Errorf("tls handshake: %w", err)
	}
	_ = tlsConn.SetDeadline(time.Time{})

	sess := mux.NewSession(tlsConn, c.sealer, false)
	sess.SetLimiter(c.cfg.Limiter)
	sess.RemoteAddr = raw.RemoteAddr().String()
	sess.ClientID = c.cfg.ClientID
	sess.OnChannel = func(ch *mux.Channel) {
		if c.OnChannel != nil {
			c.OnChannel(sess, ch)
		} else {
			_ = ch.Close()
		}
	}

	authCh := make(chan bool, 1)
	sess.OnAuthDone = func(ok bool) { authCh <- ok }

	if err := sess.SendAuth(c.cfg.Password, c.cfg.ClientID, "mytun-2"); err != nil {
		_ = sess.Close()
		return fmt.Errorf("auth send: %w", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- sess.Run() }()

	select {
	case ok := <-authCh:
		if !ok {
			_ = sess.Close()
			return errors.New("authentication failed (password mismatch)")
		}
	case err := <-runErr:
		return fmt.Errorf("session ended during auth: %w", err)
	case <-time.After(15 * time.Second):
		_ = sess.Close()
		return errors.New("authentication timeout")
	}

	c.mu.Lock()
	c.session = sess
	c.attempts = 0
	c.lastErr = ""
	c.retryAt = time.Time{}
	c.mu.Unlock()
	c.setStatus(StatusConnected, "")
	go sess.KeepAlive(25 * time.Second)
	if c.OnSession != nil {
		c.OnSession(sess)
	}

	err = <-runErr
	c.mu.Lock()
	if c.session == sess {
		c.session = nil
	}
	c.mu.Unlock()
	if c.OnSessionClosed != nil {
		c.OnSessionClosed()
	}
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return errors.New("connection closed")
}

// Manager supervises a set of client links.
type Manager struct {
	mu    sync.RWMutex
	conns map[string]*ClientConn

	OnSession       func(*mux.Session)
	OnSessionClosed func()
	OnChannel       func(*mux.Session, *mux.Channel)
	OnStatus        func(id, status, msg string)

	active atomic.Int32
}

// NewManager creates an empty manager.
func NewManager() *Manager {
	return &Manager{conns: map[string]*ClientConn{}}
}

// Add registers and starts a link.
func (m *Manager) Add(cfg ClientConnConfig) (*ClientConn, error) {
	c, err := NewClientConn(cfg)
	if err != nil {
		return nil, err
	}
	c.OnSession = func(s *mux.Session) {
		m.active.Add(1)
		if m.OnSession != nil {
			m.OnSession(s)
		}
	}
	c.OnSessionClosed = func() {
		m.active.Add(-1)
		if m.OnSessionClosed != nil {
			m.OnSessionClosed()
		}
	}
	c.OnChannel = func(s *mux.Session, ch *mux.Channel) {
		if m.OnChannel != nil {
			m.OnChannel(s, ch)
		}
	}
	c.OnStatus = func(id, status, msg string) {
		if m.OnStatus != nil {
			m.OnStatus(id, status, msg)
		}
	}
	m.mu.Lock()
	if old, ok := m.conns[cfg.ID]; ok {
		old.Stop()
	}
	m.conns[cfg.ID] = c
	m.mu.Unlock()
	c.Start()
	return c, nil
}

// Remove stops and deletes a link.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	c, ok := m.conns[id]
	delete(m.conns, id)
	m.mu.Unlock()
	if ok {
		c.Stop()
	}
}

// StopAll terminates every link.
func (m *Manager) StopAll() {
	m.mu.Lock()
	conns := m.conns
	m.conns = map[string]*ClientConn{}
	m.mu.Unlock()
	for _, c := range conns {
		c.Stop()
	}
}

// Session finds a live session by link id, falling back to the only live one.
func (m *Manager) Session(linkID string) *mux.Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if linkID != "" {
		if c, ok := m.conns[linkID]; ok {
			return c.Session()
		}
	}
	var only *mux.Session
	count := 0
	for _, c := range m.conns {
		if s := c.Session(); s != nil {
			only = s
			count++
		}
	}
	if count == 1 {
		return only
	}
	return nil
}

// States returns UI snapshots for all links.
func (m *Manager) States() []map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]map[string]any, 0, len(m.conns))
	for _, c := range m.conns {
		out = append(out, c.State())
	}
	return out
}
