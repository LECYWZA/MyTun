//go:build linux

package sysproxy

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// gsettings is the portable knob on GNOME-family desktops. On headless
// machines it is simply absent and Enable returns a clear error, so the UI can
// tell the user to configure their proxy tool instead.
var gsettingsBin = "gsettings"

func run(args ...string) (string, error) {
	out, err := exec.Command(gsettingsBin, args...).Output()
	return strings.TrimSpace(string(out)), err
}

func hasGsettings() bool {
	// Android exposes Linux syscalls but no GSettings daemon; its "system proxy"
	// is a per-Wi-Fi setting that apps cannot own, so we report unsupported.
	if _, err := os.Stat("/system/build.prop"); err == nil {
		return false
	}
	_, err := exec.LookPath(gsettingsBin)
	return err == nil
}

func gset(schema, key, value string) error {
	return exec.Command(gsettingsBin, "set", schema, key, value).Run()
}

func gget(schema, key string) string {
	v, err := run("get", schema, key)
	if err != nil {
		return ""
	}
	return strings.Trim(v, "'\"")
}

// Enable sets the GNOME system proxy to the given endpoint.
func Enable(host string, port int) error {
	if !hasGsettings() {
		return errNoGsettings
	}
	if _, ok := loadBackup(); !ok {
		_ = saveBackup(Backup{
			Enabled:   gget("org.gnome.system.proxy", "mode") == "manual",
			Server:    gget("org.gnome.system.proxy.http", "host") + ":" + gget("org.gnome.system.proxy.http", "port"),
			Bypass:    gget("org.gnome.system.proxy", "ignore-hosts"),
			Timestamp: timeNow(),
			Platform:  runtime.GOOS,
		})
	}
	if host == "" {
		host = "127.0.0.1"
	}
	p := itoa(port)
	if err := gset("org.gnome.system.proxy.http", "host", host); err != nil {
		return err
	}
	if err := gset("org.gnome.system.proxy.http", "port", p); err != nil {
		return err
	}
	_ = gset("org.gnome.system.proxy.https", "host", host)
	_ = gset("org.gnome.system.proxy.https", "port", p)
	_ = gset("org.gnome.system.proxy.socks", "host", host)
	_ = gset("org.gnome.system.proxy.socks", "port", p)
	_ = gset("org.gnome.system.proxy", "ignore-hosts", "['localhost', '127.0.0.0/8', '::1', '10.0.0.0/8', '172.16.0.0/12', '192.168.0.0/16']")
	return gset("org.gnome.system.proxy", "mode", "manual")
}

// Disable restores the saved state or switches the proxy off.
func Disable() error {
	if !hasGsettings() {
		return errNoGsettings
	}
	prev, ok := loadBackup()
	if !ok {
		return gset("org.gnome.system.proxy", "mode", "none")
	}
	_ = gset("org.gnome.system.proxy", "ignore-hosts", prev.Bypass)
	if prev.Enabled {
		if err := gset("org.gnome.system.proxy", "mode", "manual"); err != nil {
			return err
		}
	} else {
		if err := gset("org.gnome.system.proxy", "mode", "none"); err != nil {
			return err
		}
	}
	clearBackup()
	return nil
}

// Residue detects a manual proxy pointing at us without a pending backup.
func Residue() (bool, string) {
	if !hasGsettings() {
		return false, ""
	}
	if gget("org.gnome.system.proxy", "mode") != "manual" {
		return false, ""
	}
	host := gget("org.gnome.system.proxy.http", "host")
	port := gget("org.gnome.system.proxy.http", "port")
	if host == "" {
		return false, ""
	}
	if _, ok := loadBackup(); ok {
		return false, host + ":" + port
	}
	return true, host + ":" + port
}

// Current returns the active proxy endpoint.
func Current() string {
	if !hasGsettings() {
		return ""
	}
	if gget("org.gnome.system.proxy", "mode") != "manual" {
		return ""
	}
	return gget("org.gnome.system.proxy.http", "host") + ":" + gget("org.gnome.system.proxy.http", "port")
}

// Supported reports platform capability.
func Supported() bool { return hasGsettings() }

// Name returns a platform label.
func Name() string { return "Linux/GNOME" }

var errNoGsettings = &capErr{"gsettings not found: system proxy needs a GNOME/GTK desktop session"}

type capErr struct{ msg string }

func (e *capErr) Error() string { return e.msg }
