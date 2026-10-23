//go:build !android

package store

// Default driver: the pure-Go SQLite build. No cgo, so cross-compiling to
// Windows/macOS/Linux is a single command with CGO_ENABLED=0.
import (
	_ "modernc.org/sqlite"
)

const sqliteDriver = "sqlite"

// sqliteDSN builds a modernc.org/sqlite DSN.
func sqliteDSN(path string) string {
	return "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
}
