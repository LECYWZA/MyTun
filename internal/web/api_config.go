package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/LECYWZA/mytun/internal/store"
)

// ConfigPayload is the whole editable configuration, mirroring the shape the
// original Vue console expects.
type ConfigPayload struct {
	Servers  []store.ServerRow  `json:"servers"`
	Clients  []store.ClientRow  `json:"clients"`
	Forwards []store.ForwardRow `json:"forwards"`
	Proxies  []store.ProxyRow   `json:"proxies"`
	Settings map[string]string  `json:"settings"`
	Version  int                `json:"version"`
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
	settings := s.store.AllSettings()
	delete(settings, "web_pass")
	delete(settings, "jwt_secret")
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
		Settings: settings, Version: 2}, nil
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
		if err := s.saveConfig(&payload); err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
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

func (s *Server) saveConfig(p *ConfigPayload) error {
	// Replace semantics: rows absent from the payload are deleted.
	keepS := map[string]bool{}
	for _, r := range p.Servers {
		keepS[r.ID] = true
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
		if k == "web_pass" || k == "jwt_secret" {
			continue
		}
		_ = s.store.SetSetting(k, v)
	}
	return nil
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
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
	st["auditPolicy"] = map[string]string{
		"maxRows":    s.store.GetSetting("audit_max_rows", "50000"),
		"retainDays": s.store.GetSetting("audit_retain_days", "7"),
	}
	writeJSON(w, 200, st)
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
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad json"})
		return
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
	user := s.store.GetSetting("web_user", "admin")
	if body.Username == "" {
		body.Username = user
	}
	if !s.checkPassword(body.Username, body.OldPassword) {
		writeJSON(w, 401, map[string]any{"error": "原密码错误"})
		return
	}
	if len(body.NewPassword) < 4 {
		writeJSON(w, 400, map[string]any{"error": "新密码至少 4 位"})
		return
	}
	_ = s.store.SetSetting("web_user", body.Username)
	_ = s.store.SetSetting("web_pass", hashPassword(body.NewPassword))
	writeJSON(w, 200, map[string]any{"ok": true})
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
