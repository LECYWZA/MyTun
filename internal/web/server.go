// Package web serves the MyTun control console: REST API, websocket event
// stream and the embedded single-page frontend.
package web

import (
	"crypto/hmac"
	"crypto/tls"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LECYWZA/mytun/internal/logging"
	"github.com/LECYWZA/mytun/internal/mux"
	"github.com/LECYWZA/mytun/internal/runtime"
	"github.com/LECYWZA/mytun/internal/store"
	"github.com/gorilla/websocket"
)

//go:embed static
var staticFS embed.FS

const (
	// accessTTL is deliberately short: it is what the SPA holds in memory.
	accessTTL = time.Hour
	// refreshTTL keeps the user signed in on their own machine for a week.
	refreshTTL = 7 * 24 * time.Hour
	accessCookie  = "mytun_at"
	refreshCookie = "mytun_rt"
)

// Server is the HTTP console.
type Server struct {
	store *store.Store
	rt    *runtime.Runtime
	log   *logging.Collector

	secret []byte
	authMu sync.Mutex
	login  *loginGuard

	wsMu    sync.Mutex
	wsConns map[*wsClient]struct{}
	tickets map[string]time.Time

	srv     *http.Server
	ln      net.Listener
	dataDir string
	chat    *Chat
}

// Options configures the console.
type Options struct {
	Store   *store.Store
	Runtime *runtime.Runtime
	Log     *logging.Collector
	DataDir string
}

// New builds the console.
func New(o Options) (*Server, error) {
	s := &Server{
		store:   o.Store,
		rt:      o.Runtime,
		log:     o.Log,
		wsConns: map[*wsClient]struct{}{},
		tickets: map[string]time.Time{},
		login:   newLoginGuard(10, time.Minute, 60*time.Second),
		dataDir: o.DataDir,
	}
	secret := o.Store.GetSetting("jwt_secret", "")
	if secret == "" {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		secret = hex.EncodeToString(b)
		_ = o.Store.SetSetting("jwt_secret", secret)
	}
	s.secret = []byte(secret)
	if o.Store.GetSetting("web_user", "") == "" {
		_ = o.Store.SetSetting("web_user", "admin")
		_ = o.Store.SetSetting("web_pass", hashPassword("password"))
	}
	label := "服务端"
	if o.Store.GetSetting("mode", "server") == "client" {
		label = "客户端"
	}
	if s.dataDir != "" {
		s.chat = NewChat(filepath.Join(s.dataDir, "files"), label, func(ev string, v any) {
			s.wsBroadcast(ev, v)
		})
		runtime.AuxHandler = func(sess *mux.Session, ch *mux.Channel) bool {
			return s.chat.HandleChannel(sess, ch)
		}
	}
	return s, nil
}

// hashPassword stores "salt$hash" so two identical passwords do not collide and
// precomputed tables are useless.
func hashPassword(pw string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	return hex.EncodeToString(salt) + "$" + hashWithSalt(salt, pw)
}

func hashWithSalt(salt []byte, pw string) string {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte("::"))
	h.Write([]byte(pw))
	return hex.EncodeToString(h.Sum(nil))
}

// verifyPassword accepts both the current salted format and the legacy
// unsalted digest (so upgrades keep working).
func verifyPassword(stored, pw string) (ok bool, legacy bool) {
	if saltHex, hash, found := strings.Cut(stored, "$"); found {
		salt, err := hex.DecodeString(saltHex)
		if err != nil {
			return false, false
		}
		return subtle.ConstantTimeCompare([]byte(hashWithSalt(salt, pw)), []byte(hash)) == 1, false
	}
	sum := sha256.Sum256([]byte("mytun::" + pw))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(stored)) == 1, true
}

// StartTLS listens with the supplied certificate.
func (s *Server) StartTLS(addr string, cert tls.Certificate) error {
	mux := http.NewServeMux()
	s.routes(mux)
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln = ln
	go func() {
		if err := s.srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[web] serve tls: %v", err)
		}
	}()
	return nil
}

// Start listens on addr.
func (s *Server) Start(addr string) error {
	mux := http.NewServeMux()
	s.routes(mux)
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln = ln
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[web] serve: %v", err)
		}
	}()
	return nil
}

// Addr returns the bound address.
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Stop shuts the console down.
func (s *Server) Stop() {
	if s.srv != nil {
		_ = s.srv.Close()
	}
	s.wsMu.Lock()
	for c := range s.wsConns {
		c.close()
	}
	s.wsConns = map[*wsClient]struct{}{}
	s.wsMu.Unlock()
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)
	mux.HandleFunc("/api/refresh", s.handleRefresh)
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/local/status", s.handleLocalStatus)

	auth := s.requireAuth
	mux.Handle("/api/config", auth(http.HandlerFunc(s.handleConfig)))
	mux.Handle("/api/status", auth(http.HandlerFunc(s.handleStatus)))
	mux.Handle("/api/apply", auth(http.HandlerFunc(s.handleApply)))
	mux.Handle("/api/service/stop", auth(http.HandlerFunc(s.handleServiceStop)))
	mux.Handle("/api/change-password", auth(http.HandlerFunc(s.handleChangePassword)))
	mux.Handle("/api/system-proxy/", auth(http.HandlerFunc(s.handleSystemProxy)))
	mux.Handle("/api/settings", auth(http.HandlerFunc(s.handleSettings)))
	mux.Handle("/api/servers", auth(http.HandlerFunc(s.handleServers)))
	mux.Handle("/api/servers/", auth(http.HandlerFunc(s.handleServers)))
	mux.Handle("/api/clients", auth(http.HandlerFunc(s.handleClients)))
	mux.Handle("/api/clients/", auth(http.HandlerFunc(s.handleClients)))
	mux.Handle("/api/forwards", auth(http.HandlerFunc(s.handleForwards)))
	mux.Handle("/api/forwards/", auth(http.HandlerFunc(s.handleForwards)))
	mux.Handle("/api/proxies", auth(http.HandlerFunc(s.handleProxies)))
	mux.Handle("/api/proxies/", auth(http.HandlerFunc(s.handleProxies)))
	mux.Handle("/api/traffic-logs", auth(http.HandlerFunc(s.handleTrafficLogs)))
	mux.Handle("/api/traffic-logs/prune", auth(http.HandlerFunc(s.handlePruneLogs)))
	mux.Handle("/api/traffic-logs/export", auth(http.HandlerFunc(s.handleAuditExport)))
	mux.Handle("/api/stats/traffic", auth(http.HandlerFunc(s.handleTrafficSeries)))
	mux.Handle("/api/speed", auth(http.HandlerFunc(s.handleSpeed)))
	mux.Handle("/api/config/secret", auth(http.HandlerFunc(s.handleRevealSecret)))
	mux.Handle("/api/qos", auth(http.HandlerFunc(s.handleQoS)))
	mux.Handle("/api/test-latency", auth(http.HandlerFunc(s.handleTestLatency)))
	mux.Handle("/api/test-speed", auth(http.HandlerFunc(s.handleTestSpeed)))
	mux.Handle("/api/check-port/", auth(http.HandlerFunc(s.handleCheckPort)))
	mux.Handle("/api/chat/history", auth(http.HandlerFunc(s.handleChatHistory)))
	mux.Handle("/api/chat/send", auth(http.HandlerFunc(s.handleChatSend)))
	mux.Handle("/api/file/send", auth(http.HandlerFunc(s.handleFileSend)))
	mux.Handle("/api/files", auth(http.HandlerFunc(s.handleFiles)))
	mux.Handle("/api/files/", auth(http.HandlerFunc(s.handleFiles)))
	mux.Handle("/api/network-interfaces", auth(http.HandlerFunc(s.handleInterfaces)))
	mux.HandleFunc("/ws", s.handleWS)
	mux.Handle("/api/ws-ticket", auth(http.HandlerFunc(s.handleWSTicket)))

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return
	}
	mux.Handle("/", s.spaHandler(sub))
}

func (s *Server) spaHandler(sub fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(sub, path); err != nil {
			// SPA fallback
			r.URL.Path = "/"
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			data, err2 := fs.ReadFile(sub, "index.html")
			if err2 != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(data)
			return
		}
		if strings.HasPrefix(path, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		fileServer.ServeHTTP(w, r)
	})
}

// ---- auth ----

func (s *Server) token(user string, expires time.Time) string {
	mac := hmac.New(sha256.New, s.secret)
	payload := fmt.Sprintf("%s|%d", user, expires.Unix())
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) verifyToken(tok string) (string, bool) {
	parts := strings.SplitN(tok, ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", false
	}
	bits := strings.SplitN(string(payload), "|", 2)
	if len(bits) != 2 {
		return "", false
	}
	exp, err := strconv.ParseInt(bits[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	return bits[0], true
}

// isLocal reports whether the request comes from the same machine.
func (s *Server) isLocal(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// localBypass mirrors the console setting: loopback requests are trusted.
// Anyone sitting at the machine already has access to the database file, so a
// password prompt there is pure friction.
func (s *Server) localBypass() bool {
	return s.store.GetSetting("allow_local_bypass", "true") == "true"
}

func (s *Server) authenticated(r *http.Request) bool {
	if s.localBypass() && s.isLocal(r) {
		return true
	}
	for _, name := range []string{accessCookie, refreshCookie, "mytun_token"} {
		if c, err := r.Cookie(name); err == nil {
			if _, ok := s.verifyToken(c.Value); ok {
				return true
			}
		}
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if _, ok := s.verifyToken(strings.TrimPrefix(h, "Bearer ")); ok {
			return true
		}
	}
	// Allow basic auth for scripting/curl convenience.
	if u, p, ok := r.BasicAuth(); ok {
		if s.checkPassword(u, p) {
			return true
		}
	}
	return false
}

func (s *Server) checkPassword(user, pass string) bool {
	wantUser := s.store.GetSetting("web_user", "admin")
	wantPass := s.store.GetSetting("web_pass", "")
	if wantPass == "" {
		wantPass = hashPassword("password")
	}
	if subtle.ConstantTimeCompare([]byte(user), []byte(wantUser)) != 1 {
		return false
	}
	ok, legacy := verifyPassword(wantPass, pass)
	if ok && legacy {
		// transparently upgrade the on-disk hash
		_ = s.store.SetSetting("web_pass", hashPassword(pass))
	}
	return ok
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticated(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad json"})
		return
	}
	ip := clientIP(r)
	if !s.login.allow(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "登录尝试过于频繁，请稍后再试"})
		return
	}
	if !s.checkPassword(body.Username, body.Password) {
		s.login.fail(ip)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "用户名或密码错误"})
		return
	}
	s.login.reset(ip)
	access := s.token(body.Username, time.Now().Add(accessTTL))
	refresh := s.token(body.Username, time.Now().Add(refreshTTL))
	secure := r.TLS != nil
	http.SetCookie(w, &http.Cookie{
		Name: refreshCookie, Value: refresh, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: secure, MaxAge: int(refreshTTL.Seconds()),
	})
	http.SetCookie(w, &http.Cookie{
		Name: accessCookie, Value: access, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: secure, MaxAge: int(accessTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"accessToken": access, "expiresIn": int(accessTTL.Seconds()),
		"refreshExpiresIn": int(refreshTTL.Seconds()), "username": body.Username,
	})
}

// loginGuard throttles password attempts per client IP so a wrong password
// loop cannot hammer the console (or the CPU cost of hashing).
type loginGuard struct {
	mu       sync.Mutex
	attempts map[string]*attempts
	max      int
	window   time.Duration
	lockout  time.Duration
}

type attempts struct {
	count    int
	first    time.Time
	lockedTo time.Time
}

func newLoginGuard(max int, window, lockout time.Duration) *loginGuard {
	return &loginGuard{attempts: map[string]*attempts{}, max: max, window: window, lockout: lockout}
}

func (g *loginGuard) allow(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.attempts[ip]
	if !ok {
		return true
	}
	now := time.Now()
	if now.Before(a.lockedTo) {
		return false
	}
	if now.Sub(a.first) > g.window {
		delete(g.attempts, ip)
	}
	return true
}

func (g *loginGuard) fail(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	// Bound the map: only IPs that failed recently are interesting anyway.
	if len(g.attempts) > 4096 {
		for k, a := range g.attempts {
			if now.Sub(a.first) > g.window {
				delete(g.attempts, k)
			}
		}
	}
	a, ok := g.attempts[ip]
	if !ok || now.Sub(a.first) > g.window {
		g.attempts[ip] = &attempts{count: 1, first: now}
		return
	}
	a.count++
	if a.count >= g.max {
		a.lockedTo = now.Add(g.lockout)
		a.count = 0
		a.first = now
	}
}

func (g *loginGuard) reset(ip string) {
	g.mu.Lock()
	delete(g.attempts, ip)
	g.mu.Unlock()
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// handleRefresh exchanges a valid refresh cookie for a fresh access token, so
// the SPA never has to ask for the password again on the same machine.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
		return
	}
	if s.authenticated(r) && (s.localBypass() && s.isLocal(r)) {
		// Local machine: always fine, issue a fresh pair.
	}
	c, err := r.Cookie(refreshCookie)
	if err != nil {
		if s.localBypass() && s.isLocal(r) {
			access := s.token(s.store.GetSetting("web_user", "admin"), time.Now().Add(accessTTL))
			writeJSON(w, http.StatusOK, map[string]any{"accessToken": access, "local": true})
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "no session"})
		return
	}
	user, ok := s.verifyToken(c.Value)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "session expired"})
		return
	}
	access := s.token(user, time.Now().Add(accessTTL))
	secure := r.TLS != nil
	http.SetCookie(w, &http.Cookie{
		Name: accessCookie, Value: access, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: secure, MaxAge: int(accessTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"accessToken": access, "expiresIn": int(accessTTL.Seconds()), "username": user,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	for _, n := range []string{accessCookie, refreshCookie, "mytun_token"} {
		http.SetCookie(w, &http.Cookie{Name: n, Value: "", Path: "/", MaxAge: -1})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleLocalStatus is an unauthenticated, loopback-only summary used by the
// Android shell to render live state in its notification. Keeping it auth-free
// avoids storing the console password in the app just to draw a notification.
func (s *Server) handleLocalStatus(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if host != "127.0.0.1" && host != "::1" {
		http.Error(w, "loopback only", http.StatusForbidden)
		return
	}
	clients := make([]string, 0, 4)
	for id, sess := range s.rt.Sessions() {
		if sess.IsAuthed() {
			clients = append(clients, id)
		}
	}
	running := 0
	port := 0
	if servers, ok := s.rt.Status()["servers"].([]map[string]any); ok {
		for _, sv := range servers {
			if b, _ := sv["running"].(bool); b {
				running++
				if p, _ := sv["addr"].(string); p != "" && port == 0 {
					if _, ps, err := net.SplitHostPort(p); err == nil {
						fmt.Sscanf(ps, "%d", &port)
					}
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"online":  len(clients) > 0,
		"clients": clients,
		"count":   len(clients),
		"servers": running,
		"port":    port,
		"ts":      time.Now().Unix(),
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "mytun", "time": time.Now().Unix()})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20)).Decode(v)
}

// wsClient serialises all writes for one websocket. gorilla/websocket panics
// on concurrent writers, and both the broadcast ticker and the audit
// subscription write to the same socket — hence the dedicated writer goroutine.
type wsClient struct {
	conn *websocket.Conn
	send chan []byte
	once sync.Once
	quit chan struct{}
}

func newWSClient(c *websocket.Conn) *wsClient {
	w := &wsClient{conn: c, send: make(chan []byte, 256), quit: make(chan struct{})}
	go w.writer()
	return w
}

func (w *wsClient) writer() {
	for {
		select {
		case <-w.quit:
			return
		case msg := <-w.send:
			_ = w.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := w.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				w.close()
				return
			}
		}
	}
}

// push queues a frame; slow clients drop frames instead of stalling everyone.
func (w *wsClient) push(msg []byte) {
	select {
	case <-w.quit:
	case w.send <- msg:
	default:
	}
}

func (w *wsClient) close() {
	w.once.Do(func() {
		close(w.quit)
		_ = w.conn.Close()
	})
}

// wsBroadcast pushes an event to every connected console.
func (s *Server) wsBroadcast(event string, payload any) {
	msg, err := json.Marshal(map[string]any{"event": event, "data": payload, "ts": time.Now().UnixMilli()})
	if err != nil {
		return
	}
	s.wsMu.Lock()
	for c := range s.wsConns {
		c.push(msg)
	}
	s.wsMu.Unlock()
}

// Broadcast is exported so the entrypoint can push status updates.
func (s *Server) Broadcast(event string, payload any) { s.wsBroadcast(event, payload) }

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 8192,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.authenticated(r) {
		q := r.URL.Query()
		if !s.consumeTicket(q.Get("ticket")) {
			// legacy fallback: long-lived token in the query string
			if tok := q.Get("token"); tok == "" || !s.verifyTokenOK(tok) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := newWSClient(c)
	s.wsMu.Lock()
	s.wsConns[client] = struct{}{}
	s.wsMu.Unlock()

	id, recCh := s.log.Subscribe(256)
	stopSub := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopSub:
				return
			case rec, ok := <-recCh:
				if !ok {
					return
				}
				if msg, err := json.Marshal(map[string]any{"event": "audit", "data": rec}); err == nil {
					client.push(msg)
				}
			}
		}
	}()
	if st, err := json.Marshal(map[string]any{"event": "status", "data": s.rt.Status()}); err == nil {
		client.push(st)
	}

	// Read loop: we do not expect inbound frames, but reading is what detects
	// a dead peer and keeps the socket healthy.
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			break
		}
	}
	close(stopSub)
	s.log.Unsubscribe(id)
	s.wsMu.Lock()
	delete(s.wsConns, client)
	s.wsMu.Unlock()
	client.close()
}

// handleWSTicket issues a short-lived, single-use ticket. Browsers cannot set
// headers on a websocket handshake, and putting the long-lived session token in
// the URL leaks it into proxy logs; a 30s one-shot ticket is harmless there.
func (s *Server) handleWSTicket(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	ticket := hex.EncodeToString(b)
	s.wsMu.Lock()
	now := time.Now()
	for k, exp := range s.tickets {
		if now.After(exp) {
			delete(s.tickets, k)
		}
	}
	s.tickets[ticket] = now.Add(30 * time.Second)
	s.wsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ticket": ticket, "expiresIn": 30})
}

func (s *Server) consumeTicket(t string) bool {
	if t == "" {
		return false
	}
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	exp, ok := s.tickets[t]
	if !ok {
		return false
	}
	delete(s.tickets, t) // single use
	return time.Now().Before(exp)
}

func (s *Server) verifyTokenOK(tok string) bool {
	_, ok := s.verifyToken(tok)
	return ok
}

func (s *Server) handleInterfaces(w http.ResponseWriter, r *http.Request) {
	type iface struct {
		Name string   `json:"name"`
		Addrs []string `json:"addrs"`
	}
	var out []iface
	ifs, err := net.Interfaces()
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		var list []string
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil {
				list = append(list, ipnet.IP.String())
			}
		}
		if len(list) > 0 {
			out = append(out, iface{Name: i.Name, Addrs: list})
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) dataPath(name string) string {
	if s.dataDir == "" {
		return name
	}
	_ = os.MkdirAll(s.dataDir, 0o755)
	return filepath.Join(s.dataDir, name)
}
