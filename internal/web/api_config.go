package web

import (
	"errors"
	"fmt"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/LECYWZA/mytun/internal/store"
)

// maskedPassword is what the API returns instead of the real tunnel password.
// The UI can echo it back unchanged to mean "keep the existing secret".
const maskedPassword = "********"

// ConfigPayload is the whole editable configuration, mirroring the shape the
// original Vue console expects.
type ConfigPayload struct {
	Servers  []store.ServerRow  `json:"servers"`
	Clients  []store.ClientRow  `json:"clients"`
	Forwards []store.ForwardRow `json:"forwards"`
	Proxies  []store.ProxyRow   `json:"proxies"`
	Settings map[string]string  `json:"settings"`
	Version  int                `json:"version"`
	// Revision guards against two consoles overwriting each other.
	Revision int64 `json:"revision"`
}

func (s *Server) loadConfig() (*ConfigPayload, error) {
	servers, err := s.store.Servers()
	if err != nil {
		return nil, err
	}
	clients, err := s.store.Clients()
	if err != nil {
		return nil, err
	}
	forwards, err := s.store.Forwards()
	if err != nil {
		return nil, err
	}
	proxies, err := s.store.Proxies()
	if err != nil {
		return nil, err
	}
	// Seed a non-zero revision: 0 is reserved for "caller did not send one".
	if s.store.GetSetting("config_revision", "") == "" {
		_ = s.store.SetSetting("config_revision", "1")
	}
	rev, _ := strconv.ParseInt(s.store.GetSetting("config_revision", "1"), 10, 64)
	settings := s.store.AllSettings()
	delete(settings, "web_pass")
	delete(settings, "jwt_secret")
	// Never ship secrets to the browser: mask them and treat an unchanged mask
	// as "keep what is stored".
	for i := range servers {
		if servers[i].Password != "" {
			servers[i].Password = maskedPassword
		}
	}
	for i := range clients {
		if clients[i].Password != "" {
			clients[i].Password = maskedPassword
		}
	}
	for i := range proxies {
		// Old configs carry a single user/pass pair. Surface it as a normal
		// account row so the operator can see and extend it instead of staring
		// at an empty list while auth still uses the hidden pair.
		if len(proxies[i].AuthUsers) == 0 && (proxies[i].User != "" || proxies[i].Pass != "") {
			proxies[i].AuthUsers = []store.AuthUser{{User: proxies[i].User, Pass: proxies[i].Pass}}
			proxies[i].User, proxies[i].Pass = "", ""
		}
		maskProxyRow(&proxies[i])
	}
	// Never hand the UI a null array: it forces every consumer to special-case it.
	if servers == nil {
		servers = []store.ServerRow{}
	}
	if clients == nil {
		clients = []store.ClientRow{}
	}
	if forwards == nil {
		forwards = []store.ForwardRow{}
	}
	if proxies == nil {
		proxies = []store.ProxyRow{}
	}
	return &ConfigPayload{Servers: servers, Clients: clients, Forwards: forwards, Proxies: proxies,
		Settings: settings, Version: 2, Revision: rev}, nil
}

// maskProxyRow hides credential values before the config is sent to the UI.
func maskProxyRow(r *store.ProxyRow) {
	if r.Pass != "" {
		r.Pass = maskedPassword
	}
	for i := range r.AuthUsers {
		if r.AuthUsers[i].Pass != "" {
			r.AuthUsers[i].Pass = maskedPassword
		}
	}
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := s.loadConfig()
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, cfg)
	case http.MethodPost:
		var payload ConfigPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeJSON(w, 400, map[string]any{"error": "bad json: " + err.Error()})
			return
		}
		cur, _ := strconv.ParseInt(s.store.GetSetting("config_revision", "0"), 10, 64)
		if payload.Revision != 0 && payload.Revision != cur {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "配置已被其他窗口修改，请刷新后重试", "revision": cur,
			})
			return
		}
		if err := s.saveConfig(&payload); err != nil {
			code := http.StatusInternalServerError
			var bad invalidConfig
			if errors.As(err, &bad) {
				code = http.StatusBadRequest
			}
			writeJSON(w, code, map[string]any{"error": err.Error()})
			return
		}
		if err := s.rt.Apply(); err != nil {
			// Configuration is stored; report the live-apply problem but keep 200
			// so the UI can show which rule failed.
			s.wsBroadcast("apply_error", err.Error())
			cfg, _ := s.loadConfig()
			writeJSON(w, 200, map[string]any{"ok": true, "warning": err.Error(), "config": cfg})
			return
		}
		cfg, _ := s.loadConfig()
		writeJSON(w, 200, map[string]any{"ok": true, "config": cfg})
	default:
		writeJSON(w, 405, map[string]any{"error": "GET/POST only"})
	}
}

// keepSecret returns the stored secret when the client echoed the mask back.
func (s *Server) keepServerSecret(row *store.ServerRow) {
	if row.Password != maskedPassword {
		return
	}
	if rows, err := s.store.Servers(); err == nil {
		for _, r := range rows {
			if r.ID == row.ID {
				row.Password = r.Password
				return
			}
		}
	}
	row.Password = ""
}

func (s *Server) keepClientSecret(row *store.ClientRow) {
	if row.Password != maskedPassword {
		return
	}
	if rows, err := s.store.Clients(); err == nil {
		for _, r := range rows {
			if r.ID == row.ID {
				row.Password = r.Password
				return
			}
		}
	}
	row.Password = ""
}

func (s *Server) keepProxySecret(row *store.ProxyRow) {
	// The account list wins; drop the legacy pair so the two cannot drift.
	if len(row.AuthUsers) > 0 {
		row.User, row.Pass = "", ""
	}
	// Masked values come straight back from the UI; put the real secrets back
	// so an untouched form does not overwrite them with "********".
	var stored *store.ProxyRow
	if rows, err := s.store.Proxies(); err == nil {
		for i, r := range rows {
			if r.ID == row.ID {
				stored = &rows[i]
				break
			}
		}
	}
	if row.Pass == maskedPassword {
		if stored != nil {
			row.Pass = stored.Pass
		} else {
			row.Pass = ""
		}
	}
	// Known credentials, including the legacy single pair: a config that was
	// displayed as one account row must keep working when saved back unchanged.
	known := map[string]string{}
	if stored != nil {
		if stored.User != "" || stored.Pass != "" {
			known[stored.User] = stored.Pass
		}
		for _, prev := range stored.AuthUsers {
			known[prev.User] = prev.Pass
		}
	}
	for i := range row.AuthUsers {
		if row.AuthUsers[i].Pass != maskedPassword {
			continue
		}
		if pw, ok := known[row.AuthUsers[i].User]; ok {
			row.AuthUsers[i].Pass = pw
		} else {
			row.AuthUsers[i].Pass = ""
		}
	}
}

// invalidConfig marks errors caused by bad input (400) as opposed to storage
// failures (500).
type invalidConfig struct{ msg string }

func (e invalidConfig) Error() string { return e.msg }

func invalidf(format string, args ...any) error {
	return invalidConfig{msg: fmt.Sprintf(format, args...)}
}

func (s *Server) saveConfig(p *ConfigPayload) error {
	// Replace semantics: rows absent from the payload are deleted.
	keepS := map[string]bool{}
	// Same idea for tunnel servers: two enabled servers cannot share a port.
	seenS := map[int]string{}
	for _, r := range p.Servers {
		if !r.Enabled || r.Port <= 0 {
			continue
		}
		if prev, ok := seenS[r.Port]; ok {
			return invalidf("隧道端口 %d 被多个服务端占用（%s 与 %s）", r.Port, prev, r.Alias)
		}
		seenS[r.Port] = r.Alias
	}
	// Two outbound clients sharing one client id would keep kicking each other
	// off the same server session.
	// Same client id towards the same server would fight over one session; the
	// same id against a different server is perfectly fine.
	seenC := map[string]string{}
	for _, r := range p.Clients {
		if !r.Enabled || r.ClientID == "" {
			continue
		}
		key := r.Host + ":" + strconv.Itoa(r.Port) + "/" + r.ClientID
		if prev, ok := seenC[key]; ok {
			return invalidf("%s:%d 上的客户端标识 %q 重复（%s 与 %s），同标识会互相顶掉连接",
				r.Host, r.Port, r.ClientID, prev, r.Alias)
		}
		seenC[key] = r.Alias
	}

	// Reject duplicate listen ports early: the runtime would keep only one of
	// them and the operator would be left wondering why a rule does nothing.
	seenF := map[string]string{}
	for _, r := range p.Forwards {
		if !r.Enabled {
			continue
		}
		if r.ListenPort > 65535 || r.TargetPort > 65535 {
			return invalidf("端口 %d → %d 超出范围（1-65535）", r.ListenPort, r.TargetPort)
		}
		if r.Role == "local" {
			if r.TargetHost == "" || r.TargetPort <= 0 {
				return invalidf("本机转发 %s 缺少目标地址/端口", r.Note)
			}
			if r.Network != "" && r.Network != "tcp" && r.Network != "udp" && r.Network != "both" {
				return invalidf("本机转发 %s 的协议只能是 tcp / udp / both", r.Note)
			}
		}
		key := r.Role + ":" + strconv.Itoa(r.ListenPort)
		if prev, ok := seenF[key]; ok && r.ListenPort > 0 {
			return invalidf("端口 %d 被多条映射占用（%s 与 %s），请改成不同端口", r.ListenPort, prev, r.Note+"/"+r.ID)
		}
		seenF[key] = r.Note + "/" + r.ID
	}
	seenP := map[string]string{}
	for _, r := range p.Proxies {
		if !r.Enabled {
			continue
		}
		key := r.Role + ":" + strconv.Itoa(r.ListenPort)
		if prev, ok := seenP[key]; ok && r.ListenPort > 0 {
			return invalidf("端口 %d 被多个代理占用（%s 与 %s），请改成不同端口", r.ListenPort, prev, r.Note+"/"+r.ID)
		}
		seenP[key] = r.Note + "/" + r.ID
	}

	for _, r := range p.Servers {
		keepS[r.ID] = true
		s.keepServerSecret(&r)
		if err := s.store.UpsertServer(r); err != nil {
			return err
		}
	}
	oldS, _ := s.store.Servers()
	for _, r := range oldS {
		if !keepS[r.ID] {
			_ = s.store.DeleteServer(r.ID)
		}
	}

	keepC := map[string]bool{}
	for _, r := range p.Clients {
		keepC[r.ID] = true
		s.keepClientSecret(&r)
		if err := s.store.UpsertClient(r); err != nil {
			return err
		}
	}
	oldC, _ := s.store.Clients()
	for _, r := range oldC {
		if !keepC[r.ID] {
			_ = s.store.DeleteClient(r.ID)
		}
	}

	keepF := map[string]bool{}
	for _, r := range p.Forwards {
		keepF[r.ID] = true
		if err := s.store.UpsertForward(r); err != nil {
			return err
		}
	}
	oldF, _ := s.store.Forwards()
	for _, r := range oldF {
		if !keepF[r.ID] {
			_ = s.store.DeleteForward(r.ID)
		}
	}

	keepP := map[string]bool{}
	for _, r := range p.Proxies {
		keepP[r.ID] = true
		s.keepProxySecret(&r)
		if err := s.store.UpsertProxy(r); err != nil {
			return err
		}
	}
	oldP, _ := s.store.Proxies()
	for _, r := range oldP {
		if !keepP[r.ID] {
			_ = s.store.DeleteProxy(r.ID)
		}
	}

	for k, v := range p.Settings {
		if k == "web_pass" || k == "jwt_secret" || k == "config_revision" {
			continue
		}
		_ = s.store.SetSetting(k, v)
	}
	cur, _ := strconv.ParseInt(s.store.GetSetting("config_revision", "0"), 10, 64)
	_ = s.store.SetSetting("config_revision", strconv.FormatInt(cur+1, 10))
	return nil
}

// statusPayload is the single source of truth for the console status object.
// The REST endpoint and the websocket push must produce the same shape: when
// they drifted, the more frequent websocket message overwrote the REST fields
// with a payload that lacked them (the database path showed up blank).
func (s *Server) statusPayload() map[string]any {
	st := s.rt.Status()
	st["logDropped"] = s.log.Dropped()
	st["dbPath"] = s.store.Path()
	persisted, throttled := s.log.PersistStats()
	st["audit"] = map[string]any{
		"rows":      s.store.AuditCount(),
		"dbBytes":   s.store.DBSizeBytes(),
		"persisted": persisted,
		"throttled": throttled,
		"oldestTs":  s.store.OldestAuditTS(),
	}
	if m := s.rt.Speed(); m != nil {
		up, down := m.Current()
		st["speed"] = map[string]float64{"up": up, "down": down}
	}
	st["auditPolicy"] = map[string]string{
		"maxRows":    s.store.GetSetting("audit_max_rows", "50000"),
		"retainDays": s.store.GetSetting("audit_retain_days", "7"),
	}
	return st
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.statusPayload())
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	if err := s.rt.Apply(); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "status": s.rt.Status()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "status": s.rt.Status()})
}

func (s *Server) handleServiceStop(w http.ResponseWriter, r *http.Request) {
	s.rt.Stop()
	writeJSON(w, 200, map[string]any{"ok": true, "status": s.rt.Status()})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 200, s.store.AllSettings())
		return
	}
	// Accept any JSON scalar: an API client that sends {"audit_max_rows": 5000}
	// should work just like the UI that sends it as a string.
	var raw map[string]any
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad json"})
		return
	}
	body := make(map[string]string, len(raw))
	for k, v := range raw {
		switch t := v.(type) {
		case string:
			body[k] = t
		case float64:
			body[k] = strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			body[k] = strconv.FormatBool(t)
		case nil:
			body[k] = ""
		default:
			writeJSON(w, 400, map[string]any{"error": "setting " + k + " must be a scalar value"})
			return
		}
	}
	for k, v := range body {
		if k == "web_pass" || k == "jwt_secret" {
			continue
		}
		if err := s.store.SetSetting(k, v); err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleRevealSecret returns the real tunnel password for one row. The config
// payload only carries a mask, so the UI calls this when the user focuses a
// password field (to see or copy it) instead of shipping every secret to the
// browser on every poll.
func (s *Server) handleRevealSecret(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
		return
	}
	kind := r.URL.Query().Get("kind")
	id := r.URL.Query().Get("id")
	var pw, err = "", error(nil)
	var accts []store.AuthUser
	switch kind {
	case "server":
		var rows []store.ServerRow
		rows, err = s.store.Servers()
		for _, x := range rows {
			if x.ID == id {
				pw = x.Password
			}
		}
	case "client":
		var rows []store.ClientRow
		rows, err = s.store.Clients()
		for _, x := range rows {
			if x.ID == id {
				pw = x.Password
			}
		}
	case "proxy":
		var rows []store.ProxyRow
		rows, err = s.store.Proxies()
		for _, x := range rows {
			if x.ID == id {
				pw = x.Pass
				accts = x.AuthUsers
			}
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown kind"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	resp := map[string]any{"password": pw}
	if len(accts) > 0 {
		resp["authUsers"] = accts
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "POST only"})
		return
	}
	var body struct {
		Username    string `json:"username"`
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad json"})
		return
	}
	// The caller is already authenticated (this endpoint sits behind auth), so
	// asking for the old password again is pure friction. It stays accepted for
	// compatibility but is never required.
	user := s.store.GetSetting("web_user", "admin")
	if strings.TrimSpace(body.Username) != "" {
		user = strings.TrimSpace(body.Username)
	}
	if body.NewPassword != "" {
		if len(body.NewPassword) < 4 {
			writeJSON(w, 400, map[string]any{"error": "新密码至少 4 位"})
			return
		}
		_ = s.store.SetSetting("web_pass", hashPassword(body.NewPassword))
	}
	_ = s.store.SetSetting("web_user", user)
	writeJSON(w, 200, map[string]any{"ok": true, "username": user})
}

// ---- per-entity CRUD (used by curl and by the UI's quick toggles) ----

func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/servers/")
	switch r.Method {
	case http.MethodGet:
		rows, _ := s.store.Servers()
		writeJSON(w, 200, rows)
	case http.MethodPost, http.MethodPut:
		var row store.ServerRow
		if err := json.NewDecoder(r.Body).Decode(&row); err != nil {
			writeJSON(w, 400, map[string]any{"error": "bad json"})
			return
		}
		if row.ID == "" {
			row.ID = id
		}
		if err := s.store.UpsertServer(row); err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		_ = s.rt.Apply()
		writeJSON(w, 200, map[string]any{"ok": true})
	case http.MethodDelete:
		if err := s.store.DeleteServer(id); err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		_ = s.rt.Apply()
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeJSON(w, 405, map[string]any{"error": "unsupported"})
	}
}

func (s *Server) handleClients(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/clients/")
	switch r.Method {
	case http.MethodGet:
		rows, _ := s.store.Clients()
		writeJSON(w, 200, rows)
	case http.MethodPost, http.MethodPut:
		var row store.ClientRow
		if err := json.NewDecoder(r.Body).Decode(&row); err != nil {
			writeJSON(w, 400, map[string]any{"error": "bad json"})
			return
		}
		if row.ID == "" {
			row.ID = id
		}
		if err := s.store.UpsertClient(row); err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		_ = s.rt.Apply()
		writeJSON(w, 200, map[string]any{"ok": true})
	case http.MethodDelete:
		_ = s.store.DeleteClient(id)
		_ = s.rt.Apply()
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeJSON(w, 405, map[string]any{"error": "unsupported"})
	}
}

func (s *Server) handleForwards(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/forwards/")
	switch r.Method {
	case http.MethodGet:
		rows, _ := s.store.Forwards()
		writeJSON(w, 200, rows)
	case http.MethodPost, http.MethodPut:
		var row store.ForwardRow
		if err := json.NewDecoder(r.Body).Decode(&row); err != nil {
			writeJSON(w, 400, map[string]any{"error": "bad json"})
			return
		}
		if row.ID == "" {
			row.ID = id
		}
		if err := s.store.UpsertForward(row); err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		if errs := s.rt.Apply(); errs != nil {
			writeJSON(w, 200, map[string]any{"ok": true, "warning": errs.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case http.MethodDelete:
		_ = s.store.DeleteForward(id)
		_ = s.rt.Apply()
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeJSON(w, 405, map[string]any{"error": "unsupported"})
	}
}

func (s *Server) handleProxies(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/proxies/")
	switch r.Method {
	case http.MethodGet:
		rows, _ := s.store.Proxies()
		writeJSON(w, 200, rows)
	case http.MethodPost, http.MethodPut:
		var row store.ProxyRow
		if err := json.NewDecoder(r.Body).Decode(&row); err != nil {
			writeJSON(w, 400, map[string]any{"error": "bad json"})
			return
		}
		if row.ID == "" {
			row.ID = id
		}
		if err := s.store.UpsertProxy(row); err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		if errs := s.rt.Apply(); errs != nil {
			writeJSON(w, 200, map[string]any{"ok": true, "warning": errs.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case http.MethodDelete:
		_ = s.store.DeleteProxy(id)
		_ = s.rt.Apply()
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeJSON(w, 405, map[string]any{"error": "unsupported"})
	}
}

func (s *Server) handleCheckPort(w http.ResponseWriter, r *http.Request) {
	portStr := strings.TrimPrefix(r.URL.Path, "/api/check-port/")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad port"})
		return
	}
	ln, err := listenCheck(port)
	if err != nil {
		writeJSON(w, 200, map[string]any{"port": port, "available": false, "error": err.Error()})
		return
	}
	_ = ln.Close()
	writeJSON(w, 200, map[string]any{"port": port, "available": true})
}

// handleQoS reads or updates the fairness scheduler policy.
//
// Settings keys: qos_enabled, qos_mode, qos_total_kbps, qos_interactive_percent,
// qos_interactive_floor_kbps. They live in the settings table so they survive
// restarts and are exported with the rest of the configuration.
func (s *Server) handleQoS(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.rt.QoSScheduler().Stats())
	case http.MethodPost:
		var body struct {
			Enabled             *bool   `json:"enabled"`
			Mode                *string `json:"mode"`
			TotalKbps           *int    `json:"totalKbps"`
			InteractivePercent  *int    `json:"interactivePercent"`
			InteractiveFloorKbps *int   `json:"interactiveFloorKbps"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, 400, map[string]any{"error": "bad json"})
			return
		}
		set := func(k, v string) { _ = s.store.SetSetting(k, v) }
		if body.Enabled != nil {
			if *body.Enabled {
				set("qos_enabled", "true")
			} else {
				set("qos_enabled", "false")
			}
		}
		if body.Mode != nil && (*body.Mode == "auto" || *body.Mode == "fixed") {
			set("qos_mode", *body.Mode)
		}
		if body.TotalKbps != nil && *body.TotalKbps >= 0 {
			set("qos_total_kbps", strconv.Itoa(*body.TotalKbps))
		}
		if body.InteractivePercent != nil && *body.InteractivePercent > 0 && *body.InteractivePercent <= 90 {
			set("qos_interactive_percent", strconv.Itoa(*body.InteractivePercent))
		}
		if body.InteractiveFloorKbps != nil && *body.InteractiveFloorKbps > 0 {
			set("qos_interactive_floor_kbps", strconv.Itoa(*body.InteractiveFloorKbps))
		}
		if err := s.rt.Apply(); err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "qos": s.rt.QoSScheduler().Stats()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "qos": s.rt.QoSScheduler().Stats()})
	default:
		writeJSON(w, 405, map[string]any{"error": "GET/POST only"})
	}
}
