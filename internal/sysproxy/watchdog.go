package sysproxy

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// WatchdogURL is how the watchdog decides the main process is gone: it polls
// the local console health endpoint, which only exists while MyTun runs.
//
// Why a separate process instead of a defer or a signal handler: kill -9, Task
// Manager "End task", OOM kills and crashes never run deferred clean-up. A
// leftover ProxyEnable=1 pointing at a dead port takes the whole machine
// offline, which is exactly the failure mode this guards against.
const watchdogFlag = "--watchdog"

// StartWatchdog launches a detached copy of this binary in watchdog mode.
// Safe to call repeatedly: a lock file ensures only one instance runs.
func StartWatchdog(dataDir string, port int) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	lock := filepath.Join(dataDir, "watchdog.lock")
	if _, err := os.Stat(lock); err == nil {
		// Another watchdog is already supervising this installation.
		return nil
	}
	cmd := exec.Command(exe, watchdogFlag, fmt.Sprintf("http://127.0.0.1:%d/api/health", port), dataDir)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // reap when it exits
	return nil
}

// IsWatchdogArgs reports whether this process was started as a watchdog.
func IsWatchdogArgs(args []string) (healthURL string, dataDir string, ok bool) {
	for i, a := range args {
		if a == watchdogFlag && i+2 < len(args) {
			return args[i+1], args[i+2], true
		}
	}
	return "", "", false
}

// RunWatchdog blocks until the main process is gone, then restores the system
// proxy. It removes its own lock file on exit.
func RunWatchdog(healthURL, dataDir string) {
	lock := filepath.Join(dataDir, "watchdog.lock")
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return // another watchdog owns this install
	}
	_, _ = fmt.Fprintf(f, "%d", os.Getpid())
	_ = f.Close()
	defer os.Remove(lock)

	client := &http.Client{Timeout: 3 * time.Second}
	// Grace period: the console may still be starting.
	time.Sleep(10 * time.Second)

	failures := 0
	for {
		time.Sleep(3 * time.Second)
		resp, err := client.Get(healthURL)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				failures = 0
				continue
			}
		}
		failures++
		if failures < 3 {
			continue
		}
		// The main process is gone. If we still own a backup, put the user's
		// original proxy settings back so the machine keeps working.
		if _, ok := loadBackup(); ok {
			_ = Disable()
		}
		return
	}
}
