//go:build !windows

package sysproxy

// HideConsole is a no-op outside Windows (a background process without a
// terminal does not own a console window).
func HideConsole() {}

// DisableQuickEdit is a no-op outside Windows: other terminals do not freeze a
// program while text is selected.
func DisableQuickEdit() bool { return false }

// ConsoleMode reports the terminal mode; non-Windows platforms have none.
func ConsoleMode() (uint32, bool) { return 0, false }
