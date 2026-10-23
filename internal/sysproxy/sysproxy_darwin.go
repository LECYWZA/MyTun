//go:build darwin

package sysproxy

import (
	"os/exec"
	"runtime"
	"strings"
)

func serviceName() string {
	out, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return "Wi-Fi"
	}
	for _, line := range strings.Split(string(out), "\n")[1:] {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "*") {
			continue
		}
		if strings.Contains(strings.ToLower(line), "wi-fi") || strings.Contains(strings.ToLower(line), "ethernet") {
			return line
		}
	}
	return "Wi-Fi"
}

func getWebProxy(svc string) (string, string) {
	out, _ := exec.Command("networksetup", "-getwebproxy", svc).Output()
	enabled, host, port := "No", "", ""
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "Enabled":
			enabled = v
		case "Server":
			host = v
		case "Port":
			port = v
		}
	}
	return enabled + "|" + host, port
}

// Enable configures the macOS web proxy.
func Enable(host string, port int) error {
	svc := serviceName()
	if _, ok := loadBackup(); !ok {
		enabled, p := getWebProxy(svc)
		parts := strings.SplitN(enabled, "|", 2)
		_ = saveBackup(Backup{Enabled: parts[0] == "Yes", Server: parts[1] + ":" + p,
			Bypass: "", Timestamp: timeNow(), Platform: runtime.GOOS})
	}
	if host == "" {
		host = "127.0.0.1"
	}
	p := itoa(port)
	if err := exec.Command("networksetup", "-setwebproxy", svc, host, p).Run(); err != nil {
		return err
	}
	_ = exec.Command("networksetup", "-setsecurewebproxy", svc, host, p).Run()
	_ = exec.Command("networksetup", "-setsocksfirewallproxy", svc, host, p).Run()
	return exec.Command("networksetup", "-setwebproxystate", svc, "on").Run()
}

// Disable restores the previous macOS proxy state.
func Disable() error {
	svc := serviceName()
	prev, ok := loadBackup()
	if ok && prev.Enabled && prev.Server != "" {
		if h, p, err := strings.Cut(prev.Server, ":"); err {
			_ = exec.Command("networksetup", "-setwebproxy", svc, h, p).Run()
		}
		clearBackup()
		return exec.Command("networksetup", "-setwebproxystate", svc, "on").Run()
	}
	clearBackup()
	_ = exec.Command("networksetup", "-setsecurewebproxystate", svc, "off").Run()
	_ = exec.Command("networksetup", "-setsocksfirewallproxystate", svc, "off").Run()
	return exec.Command("networksetup", "-setwebproxystate", svc, "off").Run()
}

// Residue detects a leftover proxy.
func Residue() (bool, string) {
	svc := serviceName()
	state, port := getWebProxy(svc)
	if strings.HasPrefix(state, "Yes") && port != "" {
		if _, ok := loadBackup(); ok {
			return false, strings.TrimPrefix(state, "Yes|") + ":" + port
		}
		return true, strings.TrimPrefix(state, "Yes|") + ":" + port
	}
	return false, ""
}

// Current returns the active proxy endpoint.
func Current() string {
	state, port := getWebProxy(serviceName())
	if strings.HasPrefix(state, "Yes") {
		return strings.TrimPrefix(state, "Yes|") + ":" + port
	}
	return ""
}

// Supported reports platform capability.
func Supported() bool { return true }

// Name returns a platform label.
func Name() string { return "macOS" }
