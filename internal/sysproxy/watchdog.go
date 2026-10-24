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

// lockFreshness is how long a lock file stays "alive" without a refresh. The
// watchdog rewrites it every heartbeatInterval.
const (
	lockFreshness     = 20 * time.Second
	heartbeatInterval = 5 * time.Second
)

// StartWatchdog launches a detached copy of this binary in watchdog mode.
// Safe to call repeatedly: a lock file ensures only one instance runs.
func StartWatchdog(dataDir string, port int) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	lock := filepath.Join(dataDir, "watchdog.lock")
	// A PID alone is not a reliable identity: Windows and Linux both reuse PIDs,
	// so a crashed watchdog's id can land on an unrelated process and make us
	// think supervision is still running. The lock carries a heartbeat instead.
	if fi, err := os.Stat(lock); err == nil && time.Since(fi.ModTime()) < lockFreshness {
		return nil // a live watchdog refreshed it recently
	}
	_ = os.Remove(lock) // stale lock from a crashed watchdog
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

func readLockPID(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n := 0
	for _, c := range string(b) {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n, n > 0
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

	// Keep the lock fresh so a future StartWatchdog can tell "running" from
	// "crashed and left a stale file" without trusting PID reuse.
	hbDone := make(chan struct{})
	defer close(hbDone)
	go func() {
		t := time.NewTicker(heartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-hbDone:
				return
			case <-t.C:
				now := time.Now()
				_ = os.Chtimes(lock, now, now)
			}
		}
	}()

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
