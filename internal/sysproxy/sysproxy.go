// Package sysproxy toggles the operating-system HTTP proxy with a crash-safe
// backup/restore model.
//
// The reason this needs care: a proxy setting left behind after a crash makes
// the whole machine look offline. Every Enable() writes the previous value to
// disk first, Disable() restores it, and Residue() lets the UI detect a
// leftover pointing at MyTun on the next start.
package sysproxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Backup is the persisted previous state.
type Backup struct {
	Enabled   bool      `json:"enabled"`
	Server    string    `json:"server"`
	Bypass    string    `json:"bypass"`
	Timestamp time.Time `json:"timestamp"`
	Platform  string    `json:"platform"`
}

// DefaultBypass keeps local and link-local traffic off the proxy.
const DefaultBypass = "localhost,127.0.0.1,::1,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,*.local"

// dir returns the directory used for the backup file.
func dir() string {
	if d := os.Getenv("MYTUN_HOME"); d != "" {
		return d
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "MyTun")
	}
	return ".mytun"
}

func backupPath() string { return filepath.Join(dir(), "sysproxy-backup.json") }

func saveBackup(b Backup) error {
	if err := os.MkdirAll(dir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(backupPath(), data, 0o644)
}

func loadBackup() (Backup, bool) {
	var b Backup
	data, err := os.ReadFile(backupPath())
	if err != nil {
		return b, false
	}
	if json.Unmarshal(data, &b) != nil {
		return b, false
	}
	return b, true
}

func clearBackup() { _ = os.Remove(backupPath()) }

func timeNow() time.Time { return time.Now() }

// ServerString renders host:port the way each platform expects.
func ServerString(host string, port int) string {
	if host == "" {
		host = "127.0.0.1"
	}
	return host + ":" + itoa(port)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
