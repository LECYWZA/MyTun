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
