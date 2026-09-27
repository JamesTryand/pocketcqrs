package events

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestIsUnavailableRecognizesALockedStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	// a second writer that does not wait at all, against a held write lock
	holder, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	conn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(ctx, "ROLLBACK")

	impatient, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer impatient.Close()
	_, busyErr := impatient.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('x', 'y')`)
	if busyErr == nil {
		t.Fatal("expected the write to fail while another writer holds the lock")
	}
	if !IsUnavailable(busyErr) {
		t.Fatalf("SQLITE_BUSY must be recognized as unavailable: %v", busyErr)
	}

	// ordinary failures are not
	if IsUnavailable(errors.New("upcaster exploded")) || IsUnavailable(ErrConcurrency) || IsUnavailable(nil) {
		t.Fatal("only BUSY/LOCKED count as unavailable")
	}
}
