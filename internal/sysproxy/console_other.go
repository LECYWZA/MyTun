//go:build !windows

package sysproxy

// HideConsole is a no-op outside Windows (a background process without a
// terminal does not own a console window).
func HideConsole() {}
