// Package store persists MyTun configuration and audit records in SQLite.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

)

// ServerRow is one tunnel listener configuration.
type ServerRow struct {
	ID         string `json:"id"`
	Alias      string `json:"alias"`
	BindHost   string `json:"bindHost"`
	Port       int    `json:"port"`
	Password   string `json:"password"`
	CommonName string `json:"commonName"`
	Enabled    bool   `json:"enabled"`
}

// ClientRow is one upstream tunnel configuration.
type ClientRow struct {
	ID       string `json:"id"`
	Alias    string `json:"alias"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Password string `json:"password"`
	ClientID string `json:"clientId"`
	SNI      string `json:"sni"`
	Enabled  bool   `json:"enabled"`
}

// ForwardRow is one port mapping.
type ForwardRow struct {
	ID             string `json:"id"`
	Role           string `json:"role"`
	ListenHost     string `json:"listenHost"`
	ListenPort     int    `json:"listenPort"`
	TargetHost     string `json:"targetHost"`
	TargetPort     int    `json:"targetPort"`
	TargetClientID string `json:"targetClientId"`
	Enabled        bool   `json:"enabled"`
	Note           string `json:"note"`
}

// ProxyRow is one mixed proxy listener.
type ProxyRow struct {
	ID             string `json:"id"`
	Role           string `json:"role"`
	ListenHost     string `json:"listenHost"`
	ListenPort     int    `json:"listenPort"`
	NetworkMode    string `json:"networkMode"`
	TargetClientID string `json:"targetClientId"`
	UseAuth        bool   `json:"useAuth"`
	User           string `json:"user"`
	Pass           string `json:"pass"`
	AllowIPs       string `json:"allowIps"`
	DenyIPs        string `json:"denyIps"`
	Enabled        bool   `json:"enabled"`
	Note           string `json:"note"`
}

// AuditRow is one audit record.
type AuditRow struct {
	ID          int64  `json:"id"`
	TS          int64  `json:"ts"`
	Module      string `json:"module"`
	Source      string `json:"source"`
	Target      string `json:"target"`
	Action      string `json:"action"`
	NetworkMode string `json:"networkMode"`
	BytesUp     int64  `json:"bytesUp"`
	BytesDown   int64  `json:"bytesDown"`
	DurationMS  int64  `json:"durationMs"`
	Status      string `json:"status"`
	Error       string `json:"error"`
	RuleID      string `json:"ruleId"`
}

// Store is the SQLite-backed configuration store.
type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

// Open creates/opens the database at path and applies migrations.
func Open(path string) (*Store, error) {
	if path == "" {
		path = "mytun.db"
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	db, err := sql.Open(sqliteDriver, sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite writes are serialized anyway
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS settings (k TEXT PRIMARY KEY, v TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS servers (
			id TEXT PRIMARY KEY, alias TEXT, bind_host TEXT, port INTEGER,
			password TEXT, common_name TEXT, enabled INTEGER, created_at INTEGER)`,
		`CREATE TABLE IF NOT EXISTS clients (
			id TEXT PRIMARY KEY, alias TEXT, host TEXT, port INTEGER,
			password TEXT, client_id TEXT, sni TEXT, enabled INTEGER, created_at INTEGER)`,
		`CREATE TABLE IF NOT EXISTS forwards (
			id TEXT PRIMARY KEY, role TEXT, listen_host TEXT, listen_port INTEGER,
			target_host TEXT, target_port INTEGER, target_client_id TEXT,
			enabled INTEGER, note TEXT, created_at INTEGER)`,
		`CREATE TABLE IF NOT EXISTS proxies (
			id TEXT PRIMARY KEY, role TEXT, listen_host TEXT, listen_port INTEGER,
			network_mode TEXT, target_client_id TEXT, use_auth INTEGER, user TEXT,
			pass TEXT, allow_ips TEXT, deny_ips TEXT, enabled INTEGER, note TEXT, created_at INTEGER)`,
		`CREATE TABLE IF NOT EXISTS audit (
			id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER, module TEXT, source TEXT,
			target TEXT, action TEXT, network_mode TEXT, bytes_up INTEGER, bytes_down INTEGER,
			duration_ms INTEGER, status TEXT, error TEXT, rule_id TEXT)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit(ts DESC)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("store: migrate: %w (%s)", err, q[:40])
		}
	}
	return nil
}

// NewID returns a short random identifier.
func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- settings ----

// GetSetting reads a setting, returning def when absent.
func (s *Store) GetSetting(key, def string) string {
	var v string
	err := s.db.QueryRow(`SELECT v FROM settings WHERE k=?`, key).Scan(&v)
	if err != nil {
		return def
	}
	return v
}

// SetSetting writes a setting.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, key, value)
	return err
}

// AllSettings returns every setting as a map.
func (s *Store) AllSettings() map[string]string {
	out := map[string]string{}
	rows, err := s.db.Query(`SELECT k,v FROM settings`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			out[k] = v
		}
	}
	return out
}

// ---- servers ----

// Servers lists tunnel listeners.
func (s *Store) Servers() ([]ServerRow, error) {
	rows, err := s.db.Query(`SELECT id,alias,bind_host,port,password,common_name,enabled FROM servers ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServerRow
	for rows.Next() {
		var r ServerRow
		if err := rows.Scan(&r.ID, &r.Alias, &r.BindHost, &r.Port, &r.Password, &r.CommonName, &r.Enabled); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertServer inserts or updates a tunnel listener.
func (s *Store) UpsertServer(r ServerRow) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.BindHost == "" {
		r.BindHost = "0.0.0.0"
	}
	if r.CommonName == "" {
		r.CommonName = "mail.qq.com"
	}
	_, err := s.db.Exec(`INSERT INTO servers(id,alias,bind_host,port,password,common_name,enabled,created_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET alias=excluded.alias,bind_host=excluded.bind_host,port=excluded.port,
		password=excluded.password,common_name=excluded.common_name,enabled=excluded.enabled`,
		r.ID, r.Alias, r.BindHost, r.Port, r.Password, r.CommonName, r.Enabled, time.Now().UnixMilli())
	return err
}

// DeleteServer removes a listener by id.
func (s *Store) DeleteServer(id string) error {
	_, err := s.db.Exec(`DELETE FROM servers WHERE id=?`, id)
	return err
}

// ---- clients ----

// Clients lists upstream tunnel links.
func (s *Store) Clients() ([]ClientRow, error) {
	rows, err := s.db.Query(`SELECT id,alias,host,port,password,client_id,sni,enabled FROM clients ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClientRow
	for rows.Next() {
		var r ClientRow
		if err := rows.Scan(&r.ID, &r.Alias, &r.Host, &r.Port, &r.Password, &r.ClientID, &r.SNI, &r.Enabled); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertClient inserts or updates an upstream link.
func (s *Store) UpsertClient(r ClientRow) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.ClientID == "" {
		r.ClientID = "client-1"
	}
	_, err := s.db.Exec(`INSERT INTO clients(id,alias,host,port,password,client_id,sni,enabled,created_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET alias=excluded.alias,host=excluded.host,port=excluded.port,
		password=excluded.password,client_id=excluded.client_id,sni=excluded.sni,enabled=excluded.enabled`,
		r.ID, r.Alias, r.Host, r.Port, r.Password, r.ClientID, r.SNI, r.Enabled, time.Now().UnixMilli())
	return err
}

// DeleteClient removes an upstream link.
func (s *Store) DeleteClient(id string) error {
	_, err := s.db.Exec(`DELETE FROM clients WHERE id=?`, id)
	return err
}

// ---- forwards ----

// Forwards lists port mappings.
func (s *Store) Forwards() ([]ForwardRow, error) {
	rows, err := s.db.Query(`SELECT id,role,listen_host,listen_port,target_host,target_port,target_client_id,enabled,note FROM forwards ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ForwardRow
	for rows.Next() {
		var r ForwardRow
		if err := rows.Scan(&r.ID, &r.Role, &r.ListenHost, &r.ListenPort, &r.TargetHost, &r.TargetPort, &r.TargetClientID, &r.Enabled, &r.Note); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertForward inserts or updates a mapping.
func (s *Store) UpsertForward(r ForwardRow) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.Role == "" {
		r.Role = "server"
	}
	_, err := s.db.Exec(`INSERT INTO forwards(id,role,listen_host,listen_port,target_host,target_port,target_client_id,enabled,note,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET role=excluded.role,listen_host=excluded.listen_host,listen_port=excluded.listen_port,
		target_host=excluded.target_host,target_port=excluded.target_port,target_client_id=excluded.target_client_id,
		enabled=excluded.enabled,note=excluded.note`,
		r.ID, r.Role, r.ListenHost, r.ListenPort, r.TargetHost, r.TargetPort, r.TargetClientID, r.Enabled, r.Note, time.Now().UnixMilli())
	return err
}

// DeleteForward removes a mapping.
func (s *Store) DeleteForward(id string) error {
	_, err := s.db.Exec(`DELETE FROM forwards WHERE id=?`, id)
	return err
}

// ---- proxies ----

// Proxies lists mixed proxy listeners.
func (s *Store) Proxies() ([]ProxyRow, error) {
	rows, err := s.db.Query(`SELECT id,role,listen_host,listen_port,network_mode,target_client_id,use_auth,user,pass,allow_ips,deny_ips,enabled,note FROM proxies ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProxyRow
	for rows.Next() {
		var r ProxyRow
		if err := rows.Scan(&r.ID, &r.Role, &r.ListenHost, &r.ListenPort, &r.NetworkMode, &r.TargetClientID,
			&r.UseAuth, &r.User, &r.Pass, &r.AllowIPs, &r.DenyIPs, &r.Enabled, &r.Note); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertProxy inserts or updates a proxy listener.
func (s *Store) UpsertProxy(r ProxyRow) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.Role == "" {
		r.Role = "server"
	}
	if r.NetworkMode == "" {
		r.NetworkMode = "local"
	}
	_, err := s.db.Exec(`INSERT INTO proxies(id,role,listen_host,listen_port,network_mode,target_client_id,use_auth,user,pass,allow_ips,deny_ips,enabled,note,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET role=excluded.role,listen_host=excluded.listen_host,listen_port=excluded.listen_port,
		network_mode=excluded.network_mode,target_client_id=excluded.target_client_id,use_auth=excluded.use_auth,
		user=excluded.user,pass=excluded.pass,allow_ips=excluded.allow_ips,deny_ips=excluded.deny_ips,
		enabled=excluded.enabled,note=excluded.note`,
		r.ID, r.Role, r.ListenHost, r.ListenPort, r.NetworkMode, r.TargetClientID, r.UseAuth, r.User, r.Pass,
		r.AllowIPs, r.DenyIPs, r.Enabled, r.Note, time.Now().UnixMilli())
	return err
}

// DeleteProxy removes a proxy listener.
func (s *Store) DeleteProxy(id string) error {
	_, err := s.db.Exec(`DELETE FROM proxies WHERE id=?`, id)
	return err
}

// ---- audit ----

// AddAudit appends an audit row.
func (s *Store) AddAudit(r AuditRow) error {
	_, err := s.db.Exec(`INSERT INTO audit(ts,module,source,target,action,network_mode,bytes_up,bytes_down,duration_ms,status,error,rule_id)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.TS, r.Module, r.Source, r.Target, r.Action, r.NetworkMode, r.BytesUp, r.BytesDown, r.DurationMS, r.Status, r.Error, r.RuleID)
	return err
}

// AuditQuery filters audit rows.
type AuditQuery struct {
	Limit  int
	Module string
	Status string
	Target string
}

// Audit returns recent audit rows matching the query.
func (s *Store) Audit(q AuditQuery) ([]AuditRow, error) {
	if q.Limit <= 0 {
		q.Limit = 200
	}
	// Allow large pulls for CSV export; the UI simply asks for smaller pages.
	if q.Limit > 200000 {
		q.Limit = 200000
	}
	var where []string
	var args []any
	if q.Module != "" {
		where = append(where, "module=?")
		args = append(args, q.Module)
	}
	if q.Status != "" {
		where = append(where, "status=?")
		args = append(args, q.Status)
	}
	if q.Target != "" {
		where = append(where, "target LIKE ?")
		args = append(args, "%"+q.Target+"%")
	}
	sqlStr := `SELECT id,ts,module,source,target,action,network_mode,bytes_up,bytes_down,duration_ms,status,error,rule_id FROM audit`
	if len(where) > 0 {
		sqlStr += " WHERE " + strings.Join(where, " AND ")
	}
	sqlStr += " ORDER BY id DESC LIMIT ?"
	args = append(args, q.Limit)
	rows, err := s.db.Query(sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.ID, &r.TS, &r.Module, &r.Source, &r.Target, &r.Action, &r.NetworkMode,
			&r.BytesUp, &r.BytesDown, &r.DurationMS, &r.Status, &r.Error, &r.RuleID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneAudit keeps at most keep rows. Prefer PruneAuditBatched for large
// tables: a single DELETE over hundreds of thousands of rows holds the write
// lock for seconds and leaves the WAL huge.
func (s *Store) PruneAudit(keep int) error {
	_, err := s.PruneAuditBatched(keep)
	return err
}

// PruneAuditBatched deletes old rows in chunks so the database stays responsive
// while trimming. It returns how many rows were removed.
func (s *Store) PruneAuditBatched(keep int) (int64, error) {
	if keep <= 0 {
		keep = 20000
	}
	var removed int64
	const chunk = 2000
	for i := 0; i < 500; i++ { // hard stop: 1M rows per call
		// Find the id boundary of the rows we want to keep.
		var cutoff int64
		err := s.db.QueryRow(`SELECT id FROM audit ORDER BY id DESC LIMIT 1 OFFSET ?`, keep).Scan(&cutoff)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return removed, nil // fewer than keep rows
			}
			return removed, err
		}
		res, err := s.db.Exec(`DELETE FROM audit WHERE id IN (SELECT id FROM audit WHERE id <= ? ORDER BY id DESC LIMIT ?)`, cutoff, chunk)
		if err != nil {
			return removed, err
		}
		n, _ := res.RowsAffected()
		removed += n
		if n < chunk {
			return removed, nil
		}
	}
	return removed, nil
}

// AuditPolicy is the retention policy for the audit trail.
type AuditPolicy struct {
	// MaxRows caps the table size regardless of age (0 = unlimited).
	MaxRows int
	// RetainDays deletes rows older than this many days (0 = keep forever).
	RetainDays int
}

// PruneAuditPolicy applies the retention policy: first by age, then by row
// count. Both passes are chunked so a large backlog does not lock the database.
func (s *Store) PruneAuditPolicy(p AuditPolicy) (removed int64, err error) {
	if p.RetainDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -p.RetainDays).UnixMilli()
		const chunk = 2000
		for i := 0; i < 500; i++ {
			res, e := s.db.Exec(
				`DELETE FROM audit WHERE id IN (SELECT id FROM audit WHERE ts < ? ORDER BY id LIMIT ?)`,
				cutoff, chunk)
			if e != nil {
				return removed, e
			}
			n, _ := res.RowsAffected()
			removed += n
			if n < chunk {
				break
			}
		}
	}
	if p.MaxRows > 0 {
		n, e := s.PruneAuditBatched(p.MaxRows)
		removed += n
		if e != nil {
			return removed, e
		}
	}
	return removed, nil
}

// OldestAuditTS returns the timestamp of the oldest row (0 when empty).
func (s *Store) OldestAuditTS() int64 {
	var ts int64
	_ = s.db.QueryRow(`SELECT COALESCE(MIN(ts),0) FROM audit`).Scan(&ts)
	return ts
}

// TrafficBucket is one minute of aggregated traffic.
type TrafficBucket struct {
	TS        int64 `json:"ts"`
	Conns     int64 `json:"conns"`
	BytesUp   int64 `json:"bytesUp"`
	BytesDown int64 `json:"bytesDown"`
	Errors    int64 `json:"errors"`
}

// TrafficSeries aggregates the audit table into per-minute buckets.
func (s *Store) TrafficSeries(minutes int) ([]TrafficBucket, error) {
	if minutes <= 0 {
		minutes = 60
	}
	since := time.Now().Add(-time.Duration(minutes) * time.Minute).UnixMilli()
	rows, err := s.db.Query(`
		SELECT (ts/60000)*60000 AS bucket,
		       COUNT(*) AS conns,
		       COALESCE(SUM(bytes_up),0),
		       COALESCE(SUM(bytes_down),0),
		       COALESCE(SUM(CASE WHEN status!='success' THEN 1 ELSE 0 END),0)
		FROM audit WHERE ts >= ? GROUP BY bucket ORDER BY bucket ASC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TrafficBucket, 0, minutes)
	for rows.Next() {
		var b TrafficBucket
		if err := rows.Scan(&b.TS, &b.Conns, &b.BytesUp, &b.BytesDown, &b.Errors); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// AuditCount returns the number of stored audit rows.
func (s *Store) AuditCount() int64 {
	var n int64
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM audit`).Scan(&n)
	return n
}

// DBSizeBytes reports the on-disk footprint including the WAL.
func (s *Store) DBSizeBytes() int64 {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(s.path + suffix); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// Checkpoint truncates the WAL so it cannot grow without bound during long
// stretches of steady auditing.
func (s *Store) Checkpoint() error {
	_, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// Vacuum compacts the file after a large prune. Expensive; call sparingly.
func (s *Store) Vacuum() error {
	_, err := s.db.Exec(`VACUUM`)
	return err
}

// SeedDefaults creates the initial server/proxy/forward rows on a fresh DB.
func (s *Store) SeedDefaults(password string) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM servers`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if err := s.UpsertServer(ServerRow{ID: "srv-main", Alias: "默认服务端", BindHost: "0.0.0.0", Port: 33891, Password: password, CommonName: "mail.qq.com", Enabled: true}); err != nil {
			return err
		}
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM proxies`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if err := s.UpsertProxy(ProxyRow{ID: "px-main", Role: "server", ListenHost: "0.0.0.0", ListenPort: 1080,
			NetworkMode: "local", Enabled: false, Note: "混合代理 HTTP/SOCKS5"}); err != nil {
			return err
		}
	}
	return nil
}

// JSON helpers used by the web layer for slice-typed fields.
func SplitList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' || r == ';' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// MarshalList joins a list field for storage.
func MarshalList(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// ErrNotFound indicates a missing row.
var ErrNotFound = errors.New("store: not found")
