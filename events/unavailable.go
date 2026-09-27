package events

import (
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// IsUnavailable reports whether err means the event store could not be
// reached for the moment rather than that anything is wrong: SQLite's
// BUSY/LOCKED after busy_timeout ran out (another writer held the lock).
// A caller may retry such a failure; the gateway maps it to 503.
func IsUnavailable(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code() & 0xff { // primary result code; extended codes share it
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return true
	}
	return false
}
