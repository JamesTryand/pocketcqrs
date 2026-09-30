package events

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// heartbeatSchema is the writer heartbeat's table (health/telemetry contract
// section 5): one row, beside the event log so it replicates with it, never an
// event.
const heartbeatSchema = `
CREATE TABLE IF NOT EXISTS writer_heartbeat (
	id             INTEGER PRIMARY KEY CHECK (id = 1),
	writer_node_id TEXT NOT NULL,
	writer_ops_url TEXT NOT NULL,
	written_at     TEXT NOT NULL,
	sequence       INTEGER NOT NULL
);
`

// HeartbeatTimeLayout is written_at's format: UTC RFC3339 with milliseconds.
const HeartbeatTimeLayout = "2006-01-02T15:04:05.000Z07:00"

// Heartbeat is the writer heartbeat row as a reader sees it.
type Heartbeat struct {
	WriterNodeID string
	WriterOpsURL string
	WrittenAt    string
	Sequence     int64
}

// WriteHeartbeat upserts the writer heartbeat row: this writer's node id and
// ops URL, at, and a sequence one past the previous row's. It appends nothing
// to the event log. ErrReadOnly on a read-only store.
func (s *Store) WriteHeartbeat(ctx context.Context, writerNodeID, writerOpsURL string, at time.Time) error {
	if s.readOnly {
		return ErrReadOnly
	}
	// serialised with appends, like every other write to this file
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO writer_heartbeat (id, writer_node_id, writer_ops_url, written_at, sequence)
		VALUES (1, ?, ?, ?, 1)
		ON CONFLICT (id) DO UPDATE SET writer_node_id = excluded.writer_node_id,
			writer_ops_url = excluded.writer_ops_url, written_at = excluded.written_at,
			sequence = writer_heartbeat.sequence + 1`,
		writerNodeID, writerOpsURL, at.UTC().Format(HeartbeatTimeLayout))
	return err
}

// ReadHeartbeat returns the writer heartbeat row, or nil when there is none
// to see: no writer has written one yet, or this copy predates the table.
func (s *Store) ReadHeartbeat(ctx context.Context) (*Heartbeat, error) {
	var h Heartbeat
	err := s.db.QueryRowContext(ctx,
		`SELECT writer_node_id, writer_ops_url, written_at, sequence FROM writer_heartbeat WHERE id = 1`).
		Scan(&h.WriterNodeID, &h.WriterOpsURL, &h.WrittenAt, &h.Sequence)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil && strings.Contains(err.Error(), "no such table"):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return &h, nil
}
