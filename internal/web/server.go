// Package web serves the MyTun control console: REST API, websocket event
// stream and the embedded single-page frontend.
package web

import (
	"crypto/hmac"
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

// Server is the HTTP console.
type Server struct {
	store *store.Store
	rt    *runtime.Runtime
	log   *logging.Collector

	secret []byte
	authMu sync.Mutex

	wsMu    sync.Mutex
	wsConns map[*websocket.Conn]struct{}

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
		wsConns: map[*websocket.Conn]struct{}{},
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

func hashPassword(pw string) string {
	sum := sha256.Sum256([]byte("mytun::" + pw))
	return hex.EncodeToString(sum[:])
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
		_ = c.Close()
	}
	s.wsConns = map[*websocket.Conn]struct{}{}
	s.wsMu.Unlock()
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)
	mux.HandleFunc("/api/health", s.handleHealth)

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

func (s *Server) authenticated(r *http.Request) bool {
	if c, err := r.Cookie("mytun_token"); err == nil {
		if _, ok := s.verifyToken(c.Value); ok {
			return true
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
	wantPass := s.store.GetSetting("web_pass", hashPassword("password"))
	if subtle.ConstantTimeCompare([]byte(user), []byte(wantUser)) != 1 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashPassword(pass)), []byte(wantPass)) == 1
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
	if !s.checkPassword(body.Username, body.Password) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "用户名或密码错误"})
		return
	}
	tok := s.token(body.Username, time.Now().Add(7*24*time.Hour))
	http.SetCookie(w, &http.Cookie{
		Name: "mytun_token", Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 7 * 24 * 3600,
	})
	writeJSON(w, http.StatusOK, map[string]any{"accessToken": tok, "username": body.Username})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "mytun_token", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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

// wsBroadcast pushes an event to every connected console.
func (s *Server) wsBroadcast(event string, payload any) {
	msg, err := json.Marshal(map[string]any{"event": event, "data": payload, "ts": time.Now().UnixMilli()})
	if err != nil {
		return
	}
	s.wsMu.Lock()
	for c := range s.wsConns {
		_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if err := c.WriteMessage(websocket.TextMessage, msg); err != nil {
			_ = c.Close()
			delete(s.wsConns, c)
		}
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
		// Browsers cannot set headers on websocket; fall back to a query token.
		if tok := r.URL.Query().Get("token"); tok == "" || !s.verifyTokenOK(tok) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.wsMu.Lock()
	s.wsConns[c] = struct{}{}
	s.wsMu.Unlock()

	// Subscribe live audit records.
	id, recCh := s.log.Subscribe(256)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for rec := range recCh {
			msg, _ := json.Marshal(map[string]any{"event": "audit", "data": rec})
			_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
			if err := c.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	}()
	// Push an initial status snapshot.
	if st, err := json.Marshal(map[string]any{"event": "status", "data": s.rt.Status()}); err == nil {
		_ = c.WriteMessage(websocket.TextMessage, st)
	}

	for {
		if _, _, err := c.ReadMessage(); err != nil {
			break
		}
	}
	s.log.Unsubscribe(id)
	<-done
	s.wsMu.Lock()
	delete(s.wsConns, c)
	s.wsMu.Unlock()
	_ = c.Close()
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
