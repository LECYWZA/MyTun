//go:build android

package store

// Android uses the real SQLite via cgo. The pure-Go driver (modernc.org/libc)
// issues lstat (syscall 6) on amd64, which Android's seccomp policy rejects
// with SIGSYS; bionic's C library uses newfstatat instead.
import (
	_ "github.com/mattn/go-sqlite3"
)

const sqliteDriver = "sqlite3"

// sqliteDSN builds a mattn/go-sqlite3 DSN.
func sqliteDSN(path string) string {
	return "file:" + path + "?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL"
}
