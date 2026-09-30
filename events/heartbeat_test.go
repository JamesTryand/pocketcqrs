package events

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// The writer heartbeat (health/telemetry contract section 5).

func TestHeartbeatIsOneRowWhoseSequenceAdvancesAndIsNeverAnEvent(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if h, err := s.ReadHeartbeat(ctx); err != nil || h != nil {
		t.Fatalf("before any write: %+v, %v", h, err)
	}

	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	for i := range 2 {
		if err := s.WriteHeartbeat(ctx, "writer-1", "http://writer:10056", at.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	h, err := s.ReadHeartbeat(ctx)
	want := Heartbeat{WriterNodeID: "writer-1", WriterOpsURL: "http://writer:10056", WrittenAt: "2026-09-29T12:00:01.000Z", Sequence: 2}
	if err != nil || h == nil || *h != want {
		t.Fatalf("got %+v, %v; want %+v", h, err, want)
	}
	if head, _ := s.MaxPosition(ctx); head != 0 {
		t.Errorf("the heartbeat appended an event: head %d", head)
	}
}

func TestAReadOnlyCopyWithoutTheTableHasNoHeartbeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if h, err := s.ReadHeartbeat(context.Background()); err != nil || h != nil {
		t.Errorf("got %+v, %v; want none", h, err)
	}
	if err := s.WriteHeartbeat(context.Background(), "w", "http://w", time.Now()); !errors.Is(err, ErrReadOnly) {
		t.Errorf("write on a read-only store: %v", err)
	}
}
