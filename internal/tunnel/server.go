package tunnel

import (
	"crypto/subtle"
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

// ServerConfig describes one listening tunnel endpoint.
type ServerConfig struct {
	Alias     string
	BindHost  string
	Port      int
	Password  string
	CommonName string
	CertDir   string
	MaxClients int
	// Limiter shapes outbound frames; nil disables shaping.
	Limiter mux.Limiter
}

// Server is the public-side tunnel listener. A single port carries every
// virtual channel, which is the "single port, many tunnels" model.
type Server struct {
	cfg     ServerConfig
	ln      net.Listener
	sealer  *mux.Sealer
	cert    *CertPair

	mu       sync.RWMutex
	sessions map[string]*mux.Session

	// OnSession is called after a client authenticates.
	OnSession func(*mux.Session)
	// OnSessionClosed is called when an authenticated client disconnects.
	OnSessionClosed func(sess *mux.Session)
	// OnChannel handles inbound channels (forwards, messages, files).
	OnChannel func(*mux.Session, *mux.Channel)

	running  atomic.Bool
	startAt  time.Time
	connTotal atomic.Int64
	authFail  atomic.Int64
}

// NewServer builds a server; it does not listen until Start.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.BindHost == "" {
		cfg.BindHost = "0.0.0.0"
	}
	if cfg.Port <= 0 {
		return nil, errors.New("tunnel: port is required")
	}
	if strings.TrimSpace(cfg.Password) == "" {
		return nil, errors.New("tunnel: password is required")
	}
	cert, err := LoadOrCreateCert(cfg.CertDir, cfg.CommonName)
	if err != nil {
		return nil, err
	}
	sealer, err := mux.NewSealer(mux.DeriveKey(cfg.Password))
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, cert: cert, sealer: sealer, sessions: map[string]*mux.Session{}}, nil
}

// Start begins listening.
func (s *Server) Start() error {
	if s.running.Load() {
		return nil
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{s.cert.Cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"}, // http/1.1 keeps peers that sniff SNI happy
	}
	addr := net.JoinHostPort(s.cfg.BindHost, fmt.Sprint(s.cfg.Port))
	ln, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		return err
	}
	s.ln = ln
	s.running.Store(true)
	s.startAt = time.Now()
	go s.acceptLoop()
	return nil
}

// Stop closes the listener and all sessions.
func (s *Server) Stop() error {
	if !s.running.Load() {
		return nil
	}
	s.running.Store(false)
	if s.ln != nil {
		_ = s.ln.Close()
	}
	s.mu.Lock()
	for id, sess := range s.sessions {
		_ = sess.Close()
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	return nil
}

// Running reports listener state.
func (s *Server) Running() bool { return s.running.Load() }

// Addr returns the bound address.
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Stats returns counters for the UI.
func (s *Server) Stats() map[string]any {
	s.mu.RLock()
	n := len(s.sessions)
	s.mu.RUnlock()
	return map[string]any{
		"running":   s.running.Load(),
		"clients":   n,
		"connTotal": s.connTotal.Load(),
		"authFail":  s.authFail.Load(),
		"startAt":   s.startAt,
		"addr":      s.Addr(),
	}
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.running.Load() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return
		}
		s.connTotal.Add(1)
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	if tcp, ok := unwrapTCP(conn); ok {
		_ = tcp.SetNoDelay(true)
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(30 * time.Second)
	}
	sess := mux.NewSession(conn, s.sealer, true)
	sess.SetLimiter(s.cfg.Limiter)
	sess.RemoteAddr = conn.RemoteAddr().String()
	sess.OnChannel = func(ch *mux.Channel) {
		if s.OnChannel != nil {
			s.OnChannel(sess, ch)
		} else {
			_ = ch.Close()
		}
	}
	sess.OnAuth = func(password, clientID, proto string) bool {
		if subtle.ConstantTimeCompare([]byte(password), []byte(s.cfg.Password)) != 1 {
			s.authFail.Add(1)
			return false
		}
		if s.cfg.MaxClients > 0 {
			s.mu.RLock()
			n := len(s.sessions)
			s.mu.RUnlock()
			if n >= s.cfg.MaxClients {
				if old, ok := s.sessions[clientID]; !ok || old == nil {
					return false
				}
			}
		}
		s.mu.Lock()
		if old, ok := s.sessions[clientID]; ok && old != nil {
			// Replace a stale session from the same client id.
			_ = old.Close()
		}
		s.sessions[clientID] = sess
		s.mu.Unlock()
		sess.ClientID = clientID
		go sess.KeepAlive(25 * time.Second)
		if s.OnSession != nil {
			s.OnSession(sess)
		}
		return true
	}
	sess.OnClose = func(closed *mux.Session) {
		s.mu.Lock()
		if cur, ok := s.sessions[closed.ClientID]; ok && cur == closed {
			delete(s.sessions, closed.ClientID)
			s.mu.Unlock()
			if s.OnSessionClosed != nil {
				s.OnSessionClosed(closed)
			}
			return
		}
		s.mu.Unlock()
	}
	_ = sess.Run()
}

// Session returns a live authenticated session by client id.
func (s *Server) Session(clientID string) *mux.Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if clientID == "" {
		if len(s.sessions) == 1 {
			for _, v := range s.sessions {
				return v
			}
		}
		return nil
	}
	return s.sessions[clientID]
}

// Sessions returns a snapshot of authenticated sessions.
func (s *Server) Sessions() map[string]*mux.Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*mux.Session, len(s.sessions))
	for k, v := range s.sessions {
		out[k] = v
	}
	return out
}

func unwrapTCP(conn net.Conn) (*net.TCPConn, bool) {
	type tlsConn interface{ NetConn() net.Conn }
	if tc, ok := conn.(tlsConn); ok {
		if tcp, ok := tc.NetConn().(*net.TCPConn); ok {
			return tcp, true
		}
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		return tcp, true
	}
	return nil, false
}

// Matches reports whether the live listener already has the requested config.
func (s *Server) Matches(bindHost string, port int, password string) bool {
	if s == nil || !s.running.Load() {
		return false
	}
	if s.cfg.Port != port || s.cfg.Password != password {
		return false
	}
	if bindHost == "" {
		bindHost = "0.0.0.0"
	}
	return s.cfg.BindHost == bindHost
}

// Config returns the server configuration.
func (s *Server) Config() ServerConfig { return s.cfg }
