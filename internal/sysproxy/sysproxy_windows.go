//go:build windows

package sysproxy

import (
	"runtime"

	"golang.org/x/sys/windows/registry"
)

const regPath = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

func openKey() (registry.Key, error) {
	return registry.OpenKey(registry.CURRENT_USER, regPath, registry.QUERY_VALUE|registry.SET_VALUE)
}

// Enable points WinINET at the given proxy, saving the previous state first.
func Enable(host string, port int) error {
	k, err := openKey()
	if err != nil {
		return err
	}
	defer k.Close()

	// Persist the previous configuration once; repeated Enable calls keep the
	// original backup so Disable always restores the user's real settings.
	if _, ok := loadBackup(); !ok {
		prevEnabled := false
		if v, _, err := k.GetIntegerValue("ProxyEnable"); err == nil {
			prevEnabled = v == 1
		}
		prevServer, _, _ := k.GetStringValue("ProxyServer")
		prevBypass, _, _ := k.GetStringValue("ProxyOverride")
		_ = saveBackup(Backup{Enabled: prevEnabled, Server: prevServer, Bypass: prevBypass,
			Timestamp: timeNow(), Platform: runtime.GOOS})
	}

	server := ServerString(host, port)
	if err := k.SetStringValue("ProxyServer", server); err != nil {
		return err
	}
	if err := k.SetStringValue("ProxyOverride", DefaultBypass); err != nil {
		return err
	}
	if err := k.SetDWordValue("ProxyEnable", 1); err != nil {
		return err
	}
	notifyChange()
	return nil
}

// Disable restores the previous configuration (or turns the proxy off).
func Disable() error {
	k, err := openKey()
	if err != nil {
		return err
	}
	defer k.Close()

	prev, ok := loadBackup()
	if ok {
		_ = k.SetStringValue("ProxyServer", prev.Server)
		if prev.Bypass != "" {
			_ = k.SetStringValue("ProxyOverride", prev.Bypass)
		} else {
			_ = k.DeleteValue("ProxyOverride")
		}
		v := uint32(0)
		if prev.Enabled {
			v = 1
		}
		if err := k.SetDWordValue("ProxyEnable", v); err != nil {
			return err
		}
		clearBackup()
	} else {
		if err := k.SetDWordValue("ProxyEnable", 0); err != nil {
			return err
		}
	}
	notifyChange()
	return nil
}

// Residue reports whether the OS proxy still points at MyTun while no backup is
// pending — i.e. a setting left behind by a crash.
func Residue() (bool, string) {
	k, err := openKey()
	if err != nil {
		return false, err.Error()
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || v != 1 {
		return false, ""
	}
	server, _, _ := k.GetStringValue("ProxyServer")
	if server == "" {
		return false, ""
	}
	if _, ok := loadBackup(); ok {
		// We are (or were) managing it; not residue.
		return false, server
	}
	return true, server
}

// Current returns the active proxy value for display.
func Current() string {
	k, err := openKey()
	if err != nil {
		return ""
	}
	defer k.Close()
	if v, _, err := k.GetIntegerValue("ProxyEnable"); err == nil && v == 1 {
		s, _, _ := k.GetStringValue("ProxyServer")
		return s
	}
	return ""
}

// notifyChange tells WinINET to re-read the settings immediately.
func notifyChange() {
	// The registry change alone is picked up by new processes; calling
	// InternetSetOption would need wininet.dll. Instead we touch the
	// "Connections" default so Explorer refreshes sooner.
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings\Connections`, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	cur, _, err := k.GetBinaryValue("DefaultConnectionSettings")
	if err != nil || len(cur) < 9 {
		return
	}
	cp := make([]byte, len(cur))
	copy(cp, cur)
	cp[8] = 1 // "settings changed" flag
	_ = k.SetBinaryValue("DefaultConnectionSettings", cp)
}

// Supported reports platform capability.
func Supported() bool { return true }

// Name returns a human readable platform label.
func Name() string { return "Windows" }
