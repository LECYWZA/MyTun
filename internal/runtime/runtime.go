// Package runtime wires the tunnel, forwarding and proxy subsystems together
// and keeps them in sync with the SQLite configuration.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LECYWZA/mytun/internal/forward"
	"github.com/LECYWZA/mytun/internal/logging"
	"github.com/LECYWZA/mytun/internal/mux"
	"github.com/LECYWZA/mytun/internal/proxy"
	"github.com/LECYWZA/mytun/internal/qos"
	"github.com/LECYWZA/mytun/internal/speed"
	"github.com/LECYWZA/mytun/internal/store"
	"github.com/LECYWZA/mytun/internal/sysproxy"
	"github.com/LECYWZA/mytun/internal/tunnel"
)

// Options configure the runtime.
type Options struct {
	Store      *store.Store
	Log        *logging.Collector
	CertDir    string
	DataDir    string
	APIPort    int
	AutoStart  bool
}

// Runtime owns every live subsystem.
type Runtime struct {
	opts Options

	mu       sync.RWMutex
	servers  map[string]*tunnel.Server
	sessions map[string]*mux.Session // clientID -> session (server side)
	clients  *tunnel.Manager

	fwdServer *forward.Forwarder
	fwdClient *forward.Forwarder
	pxServer  *proxy.Proxy
	pxClient  *proxy.Proxy

	sched *qos.Scheduler
	spd   *speed.Meter

	sysOn atomic.Bool

	startedAt time.Time
	lastApply atomic.Int64
	applyErr  atomic.Value // string
}

// New builds a runtime.
func New(opts Options) *Runtime {
	rt := &Runtime{
		opts:     opts,
		servers:  map[string]*tunnel.Server{},
		sessions: map[string]*mux.Session{},
		clients:  tunnel.NewManager(),
	}
	// Live traffic meter: samples the mux byte counters once per second.
	rt.spd = speed.New(func() (up, down int64) {
		in, out := mux.TrafficTotals()
		return out, in // "up" = written into tunnels, "down" = read from them
	})
	resolve := rt.resolveSession
	rt.fwdServer = forward.New("server", resolve, rt.onForwardEvent)
	rt.fwdClient = forward.New("client", resolve, rt.onForwardEvent)
	rt.pxServer = proxy.New("server", resolve, rt.onProxyEvent)
	rt.pxClient = proxy.New("client", resolve, rt.onProxyEvent)
	rt.pxServer.LocalBypass = func() bool { return rt.sysOn.Load() }
	rt.pxClient.LocalBypass = func() bool { return rt.sysOn.Load() }

	rt.sched = qos.New(qos.DefaultConfig())
	rt.sched.Start()

	rt.clients.OnChannel = func(sess *mux.Session, ch *mux.Channel) {
		if rt.fwdClient.HandleChannel(sess, ch) {
			return
		}
		rt.handleAuxChannel(sess, ch)
	}
	return rt
}

// AuxHandler receives non-forward channels (chat messages/files).
var AuxHandler func(*mux.Session, *mux.Channel) bool

func (rt *Runtime) handleAuxChannel(sess *mux.Session, ch *mux.Channel) {
	if AuxHandler != nil && AuxHandler(sess, ch) {
		return
	}
	_ = ch.Close()
}

func (rt *Runtime) resolveSession(clientID string) *mux.Session {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if clientID != "" {
		if s, ok := rt.sessions[clientID]; ok {
			return s
		}
	}
	if len(rt.sessions) == 1 {
		for _, s := range rt.sessions {
			return s
		}
	}
	// Fall back to an outbound client link (reverse tunnels).
	if s := rt.clients.Session(""); s != nil {
		return s
	}
	return nil
}

func (rt *Runtime) onForwardEvent(ev forward.Event) {
	if rt.opts.Log == nil {
		return
	}
	rt.opts.Log.Add(logging.Record{
		Module:      "forward",
		Source:      ev.Source,
		Target:      ev.Target,
		Action:      ev.Role,
		BytesUp:     ev.BytesUp,
		BytesDown:   ev.BytesDown,
		DurationMS:  ev.Duration.Milliseconds(),
		Status:      ev.Status,
		Error:       ev.Error,
		RuleID:      ev.RuleID,
	})
}

func (rt *Runtime) onProxyEvent(ev proxy.Event) {
	if rt.opts.Log == nil {
		return
	}
	rt.opts.Log.Add(logging.Record{
		Module:      ev.Module,
		Source:      ev.Source,
		Target:      ev.Target,
		Action:      ev.Action,
		NetworkMode: ev.NetworkMode,
		BytesUp:     ev.BytesUp,
		BytesDown:   ev.BytesDown,
		DurationMS:  ev.Duration.Milliseconds(),
		Status:      ev.Status,
		Error:       ev.Error,
		RuleID:      ev.RuleID,
	})
}

// Start brings up every enabled component from the database.
func (rt *Runtime) Start() error {
	rt.startedAt = time.Now()
	rt.spd.Start()
	if rt.opts.AutoStart {
		return rt.Apply()
	}
	return nil
}

// Apply reconciles live state with the stored configuration.
func (rt *Runtime) Apply() error {
	if rt.opts.Store == nil {
		return errors.New("runtime: no store")
	}
	var errs []string

	// 1. Tunnel servers.
	rows, err := rt.opts.Store.Servers()
	if err != nil {
		return err
	}
	want := map[string]store.ServerRow{}
	for _, r := range rows {
		if r.Enabled {
			want[r.ID] = r
		}
	}
	// Stop stale listeners outside the lock: Stop() closes sockets and may fire
	// callbacks that would otherwise deadlock on rt.mu.
	rt.mu.Lock()
	var stale []*tunnel.Server
	for id, srv := range rt.servers {
		if _, ok := want[id]; !ok {
			stale = append(stale, srv)
			delete(rt.servers, id)
		}
	}
	rt.mu.Unlock()
	for _, srv := range stale {
		_ = srv.Stop()
	}
	if len(stale) > 0 {
		// Give the kernel a beat to release the listening sockets so a listener
		// can immediately be rebound to the same port with a different config.
		time.Sleep(120 * time.Millisecond)
	}

	for id, row := range want {
		rt.mu.RLock()
		existing := rt.servers[id]
		rt.mu.RUnlock()
		if existing != nil && existing.Running() && existing.Matches(row.BindHost, row.Port, row.Password) {
			continue
		}
		if existing != nil {
			_ = existing.Stop()
		}
		srv, err := tunnel.NewServer(tunnel.ServerConfig{
			Alias: row.Alias, BindHost: row.BindHost, Port: row.Port, Password: row.Password,
			CommonName: row.CommonName, CertDir: rt.opts.CertDir, Limiter: rt.sched,
		})
		if err != nil {
			errs = append(errs, fmt.Sprintf("server %s: %v", row.Alias, err))
			continue
		}
		srv.OnSession = func(s *mux.Session) {
			rt.mu.Lock()
			rt.sessions[s.ClientID] = s
			rt.mu.Unlock()
			log.Printf("[tunnel] client %q authenticated from %s", s.ClientID, s.RemoteAddr)
		}
		srv.OnSessionClosed = func(closed *mux.Session) {
			// Called from the session goroutine while a caller may hold rt.mu, so
			// never block the callback path synchronously.
			go func() {
				rt.mu.Lock()
				// Identity check: remove this exact session, but keep a newer one
				// that replaced it under the same client id. The previous version
				// only dropped sessions that never authenticated, so a client that
				// connected and then went away stayed "online" forever.
				if cur, ok := rt.sessions[closed.ClientID]; ok && cur == closed {
					delete(rt.sessions, closed.ClientID)
					log.Printf("[tunnel] client %q disconnected", closed.ClientID)
				}
				rt.mu.Unlock()
			}()
		}
		srv.OnChannel = func(sess *mux.Session, ch *mux.Channel) {
			if rt.fwdServer.HandleChannel(sess, ch) {
				return
			}
			rt.handleAuxChannel(sess, ch)
		}
		var startErr error
		for attempt := 0; attempt < 4; attempt++ {
			startErr = srv.Start()
			if startErr == nil {
				break
			}
			if strings.Contains(startErr.Error(), "address already in use") && attempt < 3 {
				time.Sleep(time.Duration(150*(attempt+1)) * time.Millisecond)
				continue
			}
			break
		}
		if err := startErr; err != nil {
			errs = append(errs, fmt.Sprintf("server %s listen %d: %v", row.Alias, row.Port, err))
			continue
		}
		rt.mu.Lock()
		rt.servers[id] = srv
		rt.mu.Unlock()
		log.Printf("[tunnel] server %q listening on %s", row.Alias, srv.Addr())
	}

	// 2. Outbound clients.
	clients, err := rt.opts.Store.Clients()
	if err != nil {
		return err
	}
	desired := map[string]tunnel.ClientConnConfig{}
	for _, c := range clients {
		if !c.Enabled {
			continue
		}
		desired[c.ID] = tunnel.ClientConnConfig{
			ID: c.ID, Alias: c.Alias, TunnelHost: c.Host, TunnelPort: c.Port,
			Password: c.Password, ClientID: c.ClientID, SNI: c.SNI, Limiter: rt.sched,
		}
	}
	// Remove links that are gone or changed.
	for _, st := range rt.clients.States() {
		id, _ := st["id"].(string)
		if _, ok := desired[id]; !ok {
			rt.clients.Remove(id)
			continue
		}
		host, _ := st["host"].(string)
		port, _ := st["port"].(int)
		password := ""
		for _, c := range clients {
			if c.ID == id {
				password = c.Password
			}
		}
		d := desired[id]
		if host != d.TunnelHost || port != d.TunnelPort || password != d.Password || st["clientId"] != d.ClientID {
			rt.clients.Remove(id)
		}
	}
	for id, cfg := range desired {
		found := false
		for _, st := range rt.clients.States() {
			if st["id"] == id {
				found = true
				break
			}
		}
		if !found {
			if _, err := rt.clients.Add(cfg); err != nil {
				errs = append(errs, fmt.Sprintf("client %s: %v", cfg.Alias, err))
			}
		}
	}

	// 3. Port mappings.
	fw, err := rt.opts.Store.Forwards()
	if err != nil {
		return err
	}
	var fwdRules []forward.Rule
	for _, r := range fw {
		fwdRules = append(fwdRules, forward.Rule{
			ID: r.ID, Role: r.Role, ListenHost: r.ListenHost, ListenPort: r.ListenPort,
			TargetHost: r.TargetHost, TargetPort: r.TargetPort, TargetClientID: r.TargetClientID,
			Enabled: r.Enabled,
		})
	}
	for port, e := range rt.fwdServer.Apply(filterRole(fwdRules, "server")) {
		errs = append(errs, fmt.Sprintf("forward server :%d: %s", port, e))
	}
	for port, e := range rt.fwdClient.Apply(filterRole(fwdRules, "client")) {
		errs = append(errs, fmt.Sprintf("forward client :%d: %s", port, e))
	}

	// 4. Mixed proxies.
	px, err := rt.opts.Store.Proxies()
	if err != nil {
		return err
	}
	var pxRules []proxy.Rule
	for _, r := range px {
		pxRules = append(pxRules, proxy.Rule{
			ID: r.ID, Role: r.Role, ListenHost: r.ListenHost, ListenPort: r.ListenPort,
			NetworkMode: r.NetworkMode, TargetClientID: r.TargetClientID, UseAuth: r.UseAuth,
			User: r.User, Pass: r.Pass, AllowIPs: store.SplitList(r.AllowIPs), DenyIPs: store.SplitList(r.DenyIPs),
			Enabled: r.Enabled,
		})
	}
	for port, e := range rt.pxServer.Apply(filterRolePx(pxRules, "server")) {
		errs = append(errs, fmt.Sprintf("proxy server :%d: %s", port, e))
	}
	for port, e := range rt.pxClient.Apply(filterRolePx(pxRules, "client")) {
		errs = append(errs, fmt.Sprintf("proxy client :%d: %s", port, e))
	}

	rt.applyQoS()
	rt.reconcileSystemProxy()

	rt.lastApply.Store(time.Now().UnixMilli())
	if len(errs) > 0 {
		msg := strings.Join(errs, "; ")
		rt.applyErr.Store(msg)
		return errors.New(msg)
	}
	rt.applyErr.Store("")
	return nil
}

// applyQoS rebuilds the shaping policy from settings.
func (rt *Runtime) applyQoS() {
	if rt.sched == nil || rt.opts.Store == nil {
		return
	}
	cfg := qos.DefaultConfig()
	cfg.Enabled = rt.opts.Store.GetSetting("qos_enabled", "false") == "true"
	if v := rt.opts.Store.GetSetting("qos_mode", ""); v == "fixed" {
		cfg.Mode = qos.ModeFixed
	}
	if v, err := strconv.Atoi(rt.opts.Store.GetSetting("qos_total_kbps", "0")); err == nil {
		cfg.TotalKbps = v
	}
	if v, err := strconv.Atoi(rt.opts.Store.GetSetting("qos_interactive_percent", "0")); err == nil && v > 0 {
		cfg.InteractivePercent = v
	}
	if v, err := strconv.Atoi(rt.opts.Store.GetSetting("qos_interactive_floor_kbps", "0")); err == nil && v > 0 {
		cfg.InteractiveFloorKbps = v
	}
	rt.sched.Configure(cfg)
}

// QoSScheduler exposes the shaper for the console.
func (rt *Runtime) QoSScheduler() *qos.Scheduler { return rt.sched }

func filterRole(in []forward.Rule, role string) []forward.Rule {
	out := make([]forward.Rule, 0, len(in))
	for _, r := range in {
		if r.Role == role || r.Role == "" {
			out = append(out, r)
		}
	}
	return out
}

func filterRolePx(in []proxy.Rule, role string) []proxy.Rule {
	out := make([]proxy.Rule, 0, len(in))
	for _, r := range in {
		if r.Role == role || r.Role == "" {
			out = append(out, r)
		}
	}
	return out
}

// Stop shuts everything down.
func (rt *Runtime) Stop() {
	rt.mu.Lock()
	for id, s := range rt.servers {
		_ = s.Stop()
		delete(rt.servers, id)
	}
	rt.mu.Unlock()
	rt.clients.StopAll()
	rt.fwdServer.Stop()
	rt.fwdClient.Stop()
	rt.pxServer.Stop()
	rt.pxClient.Stop()
	if rt.sched != nil {
		rt.sched.Close()
	}
	if rt.spd != nil {
		rt.spd.Stop()
	}
}

// reconcileSystemProxy keeps the OS proxy pointing at a port that is actually
// listening. After a restart the saved endpoint can be stale (the proxy was
// moved to another port), which silently breaks the machine's internet.
func (rt *Runtime) reconcileSystemProxy() {
	if !rt.sysOn.Load() {
		return
	}
	port := 0
	if st, ok := rt.Status()["proxyPorts"].(map[string]any); ok {
		for _, key := range []string{"client", "server"} {
			if list, ok := st[key].([]int); ok && len(list) > 0 {
				port = list[0]
				break
			}
		}
	}
	if port == 0 {
		if err := rt.EnableSystemProxy(false, "", 0); err == nil {
			log.Printf("[sysproxy] 没有正在运行的代理端口，已关闭系统代理避免断网")
		}
		return
	}
	saved, _ := strconv.Atoi(rt.opts.Store.GetSetting("sysproxy_port", "0"))
	if saved != port {
		if err := rt.EnableSystemProxy(true, "127.0.0.1", port); err == nil {
			log.Printf("[sysproxy] 系统代理端口已从 %d 调整为实际监听端口 %d", saved, port)
		}
	}
}

// Speed exposes the live traffic meter.
func (rt *Runtime) Speed() *speed.Meter { return rt.spd }

// Status describes live state for the UI.
func (rt *Runtime) Status() map[string]any {
	rt.mu.RLock()
	servers := make([]map[string]any, 0, len(rt.servers))
	for _, s := range rt.servers {
		st := s.Stats()
		servers = append(servers, st)
	}
	sessions := make([]map[string]any, 0, len(rt.sessions))
	for id, s := range rt.sessions {
		sessions = append(sessions, map[string]any{
			"clientId":    id,
			"remoteAddr":  s.RemoteAddr,
			"connectedAt": s.ConnectedAt,
			"channels":    s.ChannelCount(),
			"lastSeen":    s.LastActivity(),
			"role":        "inbound",
		})
	}
	rt.mu.RUnlock()

	return map[string]any{
		"servers":       servers,
		"inbound":       sessions,
		"clients":       rt.clients.States(),
		"forwardPorts":  map[string]any{"server": rt.fwdServer.Listening(), "client": rt.fwdClient.Listening()},
		"proxyPorts":    map[string]any{"server": rt.pxServer.Listening(), "client": rt.pxClient.Listening()},
		"systemProxy":   rt.sysOn.Load(),
		"startedAt":     rt.startedAt,
		"lastApply":     rt.lastApply.Load(),
		"applyError":    stringOr(rt.applyErr.Load()),
		"proxyConns":    rt.pxServer.ActiveConnections() + rt.pxClient.ActiveConnections(),
		"qos":           rt.sched.Stats(),
	}
}

func stringOr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// ServerByID returns a tunnel server instance.
func (rt *Runtime) ServerByID(id string) *tunnel.Server {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.servers[id]
}

// Sessions returns the inbound session map.
func (rt *Runtime) Sessions() map[string]*mux.Session {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	out := make(map[string]*mux.Session, len(rt.sessions))
	for k, v := range rt.sessions {
		out[k] = v
	}
	return out
}

// ClientManager exposes outbound links.
func (rt *Runtime) ClientManager() *tunnel.Manager { return rt.clients }

// EnableSystemProxy toggles the OS-level proxy.
//
// Enabling also starts a detached watchdog process: if this process is killed
// (Task Manager, kill -9, crash) the watchdog notices the console is gone and
// restores the previous proxy settings, so the machine never stays offline.
func (rt *Runtime) EnableSystemProxy(enable bool, host string, port int) error {
	if enable {
		if err := sysproxy.Enable(host, port); err != nil {
			return err
		}
		rt.sysOn.Store(true)
		if rt.opts.DataDir != "" && rt.opts.APIPort > 0 {
			if err := sysproxy.StartWatchdog(rt.opts.DataDir, rt.opts.APIPort); err != nil {
				log.Printf("[sysproxy] watchdog start failed: %v", err)
			}
		}
		return nil
	}
	if err := sysproxy.Disable(); err != nil {
		return err
	}
	rt.sysOn.Store(false)
	return nil
}

// ResumeSystemProxy re-applies the proxy state after a restart when the config
// says it should be on (used by --no-autostart=false boot paths).
func (rt *Runtime) ResumeSystemProxy(enabled bool, host string, port int) error {
	if !enabled {
		return nil
	}
	return rt.EnableSystemProxy(true, host, port)
}

// SystemProxyEnabled reports the toggle state.
func (rt *Runtime) SystemProxyEnabled() bool { return rt.sysOn.Load() }

// DetectSystemProxyResidue checks for a leftover proxy setting from a previous
// crash, so the UI can offer one-click cleanup.
func DetectSystemProxyResidue() (bool, string) {
	return sysproxy.Residue()
}

// SpeedTest measures latency (TCP connect) and download throughput through a
// given proxy rule. mode is "local" or "remote".
func (rt *Runtime) SpeedTest(ctx context.Context, ruleID, target string, timeout time.Duration) (map[string]any, error) {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	if target == "" {
		target = "www.gstatic.com:443"
	}
	start := time.Now()
	var conn net.Conn
	var err error
	if ruleID != "" {
		// Route through a configured proxy listener to exercise the same path
		// real clients use.
		var pxRule *proxy.Rule
		rows, _ := rt.opts.Store.Proxies()
		for _, r := range rows {
			if r.ID == ruleID {
				p := proxy.Rule{ID: r.ID, Role: r.Role, ListenHost: r.ListenHost, ListenPort: r.ListenPort,
					NetworkMode: r.NetworkMode, TargetClientID: r.TargetClientID,
					UseAuth: r.UseAuth, User: r.User, Pass: r.Pass}
				pxRule = &p
				break
			}
		}
		if pxRule == nil {
			return nil, fmt.Errorf("proxy rule %s not found", ruleID)
		}
		conn, err = dialViaProxy(pxRule, target, timeout)
	} else {
		d := net.Dialer{Timeout: timeout}
		conn, err = d.Dial("tcp", target)
	}
	if err != nil {
		return nil, err
	}
	latency := time.Since(start)
	_ = conn.Close()
	return map[string]any{
		"target":  target,
		"latency": latency.Milliseconds(),
		"status":  "ok",
	}, nil
}

func dialViaProxy(r *proxy.Rule, target string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	host := r.ListenHost
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	conn, err := d.Dial("tcp", net.JoinHostPort(host, fmt.Sprint(r.ListenPort)))
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	// SOCKS5 no-auth handshake.
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	resp := make([]byte, 2)
	if _, err := readFull(conn, resp); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp[1] == 0x02 {
		// username/password auth
		buf := []byte{0x01, byte(len(r.User))}
		buf = append(buf, r.User...)
		buf = append(buf, byte(len(r.Pass)))
		buf = append(buf, r.Pass...)
		if _, err := conn.Write(buf); err != nil {
			_ = conn.Close()
			return nil, err
		}
		ar := make([]byte, 2)
		if _, err := readFull(conn, ar); err != nil {
			_ = conn.Close()
			return nil, err
		}
		if ar[1] != 0x00 {
			_ = conn.Close()
			return nil, errors.New("proxy auth failed")
		}
	}
	hostName, portStr, err := net.SplitHostPort(target)
	if err != nil {
		hostName, portStr = target, "443"
	}
	port := 0
	fmt.Sscanf(portStr, "%d", &port)
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(hostName))}
	req = append(req, hostName...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reply := make([]byte, 10)
	if _, err := readFull(conn, reply); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if reply[1] != 0x00 {
		_ = conn.Close()
		return nil, fmt.Errorf("socks5 connect failed: %d", reply[1])
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func readFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := c.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
