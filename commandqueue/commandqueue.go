// Package commandqueue durably records incoming commands before they are
// decided, so a batching event writer can accumulate many commands' decided
// events into one SQLite transaction instead of one per command (item 4).
//
// Deliberately kept in its own SQLite file, separate from events.db -- the
// same reasoning as idempotency.db. This is possible because completion
// does not need to be atomic with this store at all: a decided command's
// own id, stamped into the events it produces (see events.Store.CommandApplied),
// is the durable proof it already ran. This store's own "done" marking is
// pure, eventually-consistent cleanup bookkeeping -- it can lag arbitrarily
// with zero correctness risk, because restart-recovery never consults it.
package commandqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS command_queue (
	position             INTEGER PRIMARY KEY AUTOINCREMENT,
	id                   TEXT NOT NULL UNIQUE,
	aggregate            TEXT NOT NULL,
	aggregate_id         TEXT NOT NULL,
	command              TEXT NOT NULL,
	payload              TEXT NOT NULL,
	meta                 TEXT NOT NULL DEFAULT '{}',
	enqueued_at_position INTEGER NOT NULL,
	enqueued             TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%fZ', 'now')),
	done                 INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_command_queue_pending ON command_queue (position) WHERE done = 0;
`

// QueuedCommand is one durably-recorded, not-necessarily-yet-decided command.
type QueuedCommand struct {
	Position           int64           `json:"position"`
	ID                 string          `json:"id"`
	Aggregate          string          `json:"aggregate"`
	AggregateID        string          `json:"aggregateId"`
	Command            string          `json:"command"`
	Payload            json.RawMessage `json:"payload"`
	Meta               json.RawMessage `json:"meta"`
	EnqueuedAtPosition int64           `json:"enqueuedAtPosition"`
	Enqueued           string          `json:"enqueued"`
}

// Store persists queued commands for the batching event writer.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the command queue store at path.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("commandqueue: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("commandqueue: init schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// EnqueueCommand durably records a command's intent and returns the row
// assigned to it. Its id is a UUID v7 -- time-ordered, so the index on this
// column (and the commandId index on events, see events.Store.CommandApplied)
// insert at the tail rather than scattering, unlike the project's plain
// random-hex event ids, which are never looked up by value and so have no
// such requirement.
//
// meta must already carry everything the eventual decide needs that isn't
// itself deterministic (e.g. "now", the actor) -- it is replayed unchanged
// on every decide attempt, including after a crash, never re-derived.
//
// enqueuedAtPosition is the event store's head position at the moment of
// enqueueing (the caller reads this via events.Store.MaxPosition) -- it
// bounds a later restart-recovery scan to events appended since this
// command arrived, instead of the whole log.
func (s *Store) EnqueueCommand(ctx context.Context, aggregate, aggregateID, command string, payload, meta json.RawMessage, enqueuedAtPosition int64) (QueuedCommand, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return QueuedCommand{}, fmt.Errorf("commandqueue: generate id: %w", err)
	}
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if len(meta) == 0 {
		meta = json.RawMessage(`{}`)
	}

	qc := QueuedCommand{
		ID:                 id.String(),
		Aggregate:          aggregate,
		AggregateID:        aggregateID,
		Command:            command,
		Payload:            payload,
		Meta:               meta,
		EnqueuedAtPosition: enqueuedAtPosition,
	}
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO command_queue (id, aggregate, aggregate_id, command, payload, meta, enqueued_at_position)
		 VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING position, enqueued`,
		qc.ID, qc.Aggregate, qc.AggregateID, qc.Command, string(qc.Payload), string(qc.Meta), qc.EnqueuedAtPosition,
	).Scan(&qc.Position, &qc.Enqueued)
	if err != nil {
		return QueuedCommand{}, fmt.Errorf("commandqueue: enqueue: %w", err)
	}
	return qc, nil
}

// PendingCommands returns up to limit not-yet-done commands, oldest first.
func (s *Store) PendingCommands(ctx context.Context, limit int) ([]QueuedCommand, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT position, id, aggregate, aggregate_id, command, payload, meta, enqueued_at_position, enqueued
		 FROM command_queue WHERE done = 0 ORDER BY position LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []QueuedCommand
	for rows.Next() {
		var qc QueuedCommand
		var payload, meta string
		if err := rows.Scan(&qc.Position, &qc.ID, &qc.Aggregate, &qc.AggregateID, &qc.Command,
			&payload, &meta, &qc.EnqueuedAtPosition, &qc.Enqueued); err != nil {
			return nil, err
		}
		qc.Payload = json.RawMessage(payload)
		qc.Meta = json.RawMessage(meta)
		out = append(out, qc)
	}
	return out, rows.Err()
}

// Depth reports how many enqueued commands are not yet done. Used for
// admission control (F-5): a caller sheds new commands once this crosses a
// configured ceiling, instead of letting them pile up as blocked goroutines
// with no bound but SQLite's own busy_timeout.
func (s *Store) Depth(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM command_queue WHERE done = 0`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("commandqueue: depth: %w", err)
	}
	return n, nil
}

// MarkDone marks the given command ids done. Pure bookkeeping, never
// consulted for correctness (see the package doc comment) -- safe to call
// late, out of order, or repeatedly on ids already marked done.
func (s *Store) MarkDone(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `UPDATE command_queue SET done = 1 WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
