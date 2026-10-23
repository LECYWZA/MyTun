package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/LECYWZA/mytun/internal/logging"
	"github.com/LECYWZA/mytun/internal/mux"
	"github.com/LECYWZA/mytun/internal/store"
	"github.com/LECYWZA/mytun/internal/sysproxy"
)

func (s *Server) handleTrafficLogs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		q := store.AuditQuery{
			Module: r.URL.Query().Get("module"),
			Status: r.URL.Query().Get("status"),
			Target: r.URL.Query().Get("target"),
		}
		if l := r.URL.Query().Get("limit"); l != "" {
			q.Limit, _ = strconv.Atoi(l)
		}
		rows, err := s.store.Audit(q)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		out := make([]logging.Record, 0, len(rows))
		for _, a := range rows {
			out = append(out, logging.Record{
				ID: a.ID, TS: a.TS, Module: a.Module, Source: a.Source, Target: a.Target,
				Action: a.Action, NetworkMode: a.NetworkMode, BytesUp: a.BytesUp, BytesDown: a.BytesDown,
				DurationMS: a.DurationMS, Status: a.Status, Error: a.Error, RuleID: a.RuleID,
			})
		}
		writeJSON(w, 200, out)
	case http.MethodDelete:
		_ = s.store.PruneAudit(0)
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeJSON(w, 405, map[string]any{"error": "GET/DELETE only"})
	}
}

// handlePruneLogs trims the audit table on demand (the daemon also does this on
// a timer, this endpoint just makes it observable/testable).
func (s *Server) handlePruneLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "POST only"})
		return
	}
	keep := 20000
	if v := r.URL.Query().Get("keep"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			keep = n
		}
	}
	beforeRows := s.store.AuditCount()
	beforeBytes := s.store.DBSizeBytes()
	policy := store.AuditPolicy{
		MaxRows:    keep,
		RetainDays: atoiOrDefault(r.URL.Query().Get("days"), atoiOrDefault(s.store.GetSetting("audit_retain_days", "7"), 7)),
	}
	removed, err := s.store.PruneAuditPolicy(policy)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	_ = s.store.Checkpoint()
	// Deleting rows only marks pages free; VACUUM is what actually shrinks the
	// file. Only worth it when something was really removed.
	if removed > 0 {
		_ = s.store.Vacuum()
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "removed": removed, "keep": keep, "retainDays": policy.RetainDays,
		"rowsBefore": beforeRows, "rowsAfter": s.store.AuditCount(),
		"bytesBefore": beforeBytes, "bytesAfter": s.store.DBSizeBytes(),
		"oldestTs": s.store.OldestAuditTS(),
	})
}

func (s *Server) handleSystemProxy(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimPrefix(r.URL.Path, "/api/system-proxy/")
	var body struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch action {
	case "enable":
		if body.Host == "" {
			body.Host = "127.0.0.1"
		}
		if body.Port == 0 {
			rows, _ := s.store.Proxies()
			for _, p := range rows {
				if p.Enabled && p.Role == "server" {
					body.Port = p.ListenPort
					break
				}
			}
		}
		if body.Port == 0 {
			writeJSON(w, 400, map[string]any{"error": "没有可用的代理端口，请先启用一个混合代理"})
			return
		}
		if err := s.rt.EnableSystemProxy(true, body.Host, body.Port); err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		_ = s.store.SetSetting("sysproxy_wanted", "true")
		_ = s.store.SetSetting("sysproxy_port", strconv.Itoa(body.Port))
		writeJSON(w, 200, map[string]any{"ok": true, "enabled": true, "endpoint": sysproxy.ServerString(body.Host, body.Port)})
	case "disable":
		if err := s.rt.EnableSystemProxy(false, "", 0); err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		_ = s.store.SetSetting("sysproxy_wanted", "false")
		writeJSON(w, 200, map[string]any{"ok": true, "enabled": false})
	case "status":
		residue, detail := sysproxy.Residue()
		writeJSON(w, 200, map[string]any{
			"enabled": s.rt.SystemProxyEnabled(), "current": sysproxy.Current(),
			"residue": residue, "detail": detail, "supported": sysproxy.Supported(),
			"platform": sysproxy.Name(),
		})
	default:
		writeJSON(w, 404, map[string]any{"error": "unknown action"})
	}
}

// handleTestLatency performs TCP connect latency to a target, optionally via a
// configured proxy rule.
func (s *Server) handleTestLatency(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Target string `json:"target"`
		RuleID string `json:"ruleId"`
		Timeout int   `json:"timeout"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Target == "" {
		body.Target = "www.gstatic.com:443"
	}
	to := time.Duration(body.Timeout) * time.Millisecond
	if to <= 0 || to > 30*time.Second {
		to = 8 * time.Second
	}
	res, err := s.rt.SpeedTest(r.Context(), body.RuleID, body.Target, to)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "target": body.Target})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": res})
}

// handleTestSpeed downloads a URL through the proxy and reports throughput.
func (s *Server) handleTestSpeed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL     string `json:"url"`
		RuleID  string `json:"ruleId"`
		Bytes   int64  `json:"bytes"`
		Timeout int    `json:"timeout"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.URL == "" {
		body.URL = "https://speed.cloudflare.com/__down?bytes=5000000"
	}
	if body.Bytes <= 0 {
		body.Bytes = 5 << 20
	}
	to := time.Duration(body.Timeout) * time.Millisecond
	if to <= 0 {
		to = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), to)
	defer cancel()

	tr := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext}
	if body.RuleID != "" {
		url, err := s.proxyURL(body.RuleID)
		if err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		tr.Proxy = http.ProxyURL(url)
	}
	client := &http.Client{Transport: tr}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, body.URL, nil)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	ttfb := time.Since(start)
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, body.Bytes))
	elapsed := time.Since(start)
	mbps := 0.0
	if elapsed > 0 {
		mbps = float64(n) * 8 / elapsed.Seconds() / 1e6
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": map[string]any{
		"bytes": n, "elapsedMs": elapsed.Milliseconds(), "ttfbMs": ttfb.Milliseconds(),
		"mbps": mbps, "url": body.URL, "status": resp.StatusCode,
	}})
}

func (s *Server) proxyURL(ruleID string) (*url.URL, error) {
	rows, _ := s.store.Proxies()
	for _, p := range rows {
		if p.ID != ruleID {
			continue
		}
		host := p.ListenHost
		if host == "" || host == "0.0.0.0" {
			host = "127.0.0.1"
		}
		scheme := "http"
		auth := ""
		if p.UseAuth && p.User != "" {
			auth = p.User + ":" + p.Pass + "@"
		}
		return url.Parse(fmt.Sprintf("%s://%s%s:%d", scheme, auth, host, p.ListenPort))
	}
	return nil, fmt.Errorf("proxy rule %s not found", ruleID)
}

// ---- chat ----

func (s *Server) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	if s.chat == nil {
		writeJSON(w, 200, map[string]any{"messages": []any{}, "files": []any{}})
		return
	}
	writeJSON(w, 200, map[string]any{"messages": s.chat.Messages(), "files": s.chat.Files()})
}

func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text     string `json:"text"`
		Target   string `json:"target"`
		ClientID string `json:"clientId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad json"})
		return
	}
	sess := s.anySession(body.ClientID)
	if sess == nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "隧道未连接"})
		return
	}
	msg, err := s.chat.SendText(sess, body.Text, body.Target)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": msg})
}

func (s *Server) handleFileSend(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeJSON(w, 400, map[string]any{"error": "multipart: " + err.Error()})
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	defer file.Close()
	sess := s.anySession(r.FormValue("clientId"))
	if sess == nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "隧道未连接"})
		return
	}
	tmp := filepath.Join(s.chat.ReceiveDir(), ".upload_"+hdr.Filename)
	f, err := os.Create(tmp)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	if _, err := io.Copy(f, file); err != nil {
		_ = f.Close()
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	_ = f.Close()
	rec, err := s.chat.SendFile(sess, tmp, r.FormValue("target"), hdr.Filename)
	_ = os.Remove(tmp)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "file": rec})
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/"), "/api/files")
	name = strings.TrimPrefix(name, "/")
	switch r.Method {
	case http.MethodGet:
		if name == "" {
			writeJSON(w, 200, s.chat.ListFiles())
			return
		}
		path := filepath.Join(s.chat.ReceiveDir(), filepath.Base(name))
		if _, err := os.Stat(path); err != nil {
			writeJSON(w, 404, map[string]any{"error": "not found"})
			return
		}
		w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(name)+"\"")
		http.ServeFile(w, r, path)
	case http.MethodDelete:
		path := filepath.Join(s.chat.ReceiveDir(), filepath.Base(name))
		if err := os.Remove(path); err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeJSON(w, 405, map[string]any{"error": "GET/DELETE only"})
	}
}

func (s *Server) anySession(clientID string) *mux.Session {
	if clientID != "" {
		if sess := s.rt.Sessions()[clientID]; sess != nil {
			return sess
		}
	}
	for _, sess := range s.rt.Sessions() {
		return sess
	}
	if sess := s.rt.ClientManager().Session(""); sess != nil {
		return sess
	}
	return nil
}

func atoiOrDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}
