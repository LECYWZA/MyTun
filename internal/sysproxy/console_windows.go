//go:build windows

package sysproxy

import "golang.org/x/sys/windows"

var (
	kernel32       = windows.NewLazySystemDLL("kernel32.dll")
	user32         = windows.NewLazySystemDLL("user32.dll")
	procGetConsole = kernel32.NewProc("GetConsoleWindow")
	procShowWindow = user32.NewProc("ShowWindow")
)

// HideConsole hides the console window this process owns. Used with --hide so a
// start-up shortcut does not leave a black window on screen for the whole
// session.
func HideConsole() {
	hwnd, _, _ := procGetConsole.Call()
	if hwnd == 0 {
		return
	}
	const swHide = 0
	_, _, _ = procShowWindow.Call(hwnd, uintptr(swHide))
}

// Console input mode flags we care about (not all are exported by x/sys).
const (
	enableQuickEditMode = 0x0040
	enableExtendedFlags = 0x0080
)

// DisableQuickEdit turns off the Windows console "QuickEdit" selection mode.
//
// With it on (the Windows default) a stray click in the console window puts it
// into mark/select state, and every subsequent write to stdout blocks until the
// selection is cleared — the program looks hung. Turning it off keeps output
// flowing while the window stays clickable.
var (
	lastMode uint32
	haveMode bool
)

// ConsoleMode reports the console input mode observed at start-up (0 if there
// was no real console, e.g. when stdout is redirected).
func ConsoleMode() (uint32, bool) { return lastMode, haveMode }

func DisableQuickEdit() bool {
	if err := kernel32.NewProc("SetConsoleMode").Find(); err != nil {
		return false
	}
	changed := false
	for _, name := range []string{"CONIN$", "CONOUT$"} {
		p, err := windows.UTF16PtrFromString(name)
		if err != nil {
			continue
		}
		h, err := windows.CreateFile(p,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			continue
		}
		var mode uint32
		if err := windows.GetConsoleMode(h, &mode); err == nil {
			lastMode = mode
			haveMode = true
			if mode&enableQuickEditMode != 0 {
				mode &^= enableQuickEditMode
				mode |= enableExtendedFlags
				if err := windows.SetConsoleMode(h, mode); err == nil {
					changed = true
				}
			}
		}
		_ = windows.CloseHandle(h)
	}
	return changed
}
