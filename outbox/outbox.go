// Package outbox holds the work an event asks of the world outside the
// process (an HTTP call, a key destroyed), so it runs off the engine's pass
// and is retried by due time (R4).
//
// A consumer's Apply only enqueues: its checkpoint advances at once and a
// slow or failing call never holds up the projections behind it. A Worker
// takes each entry when it is due and hands it to its consumer's Handler,
// which says whether it is done or when to try again. A retry is the entry
// rescheduled with a due time; the worker waits for the next due entry (or a
// nudge from an enqueue), never a fixed pause.
//
// Kept in its own SQLite file, as commandqueue and idempotency are: an
// entry needs no transaction with the event log or data.db, because its id
// is derived from the event (a redelivery enqueues the same entry once) and
// a handler's own work is idempotent or keyed (follow-up commands carry
// idempotency keys). One worker runs an outbox file.
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS outbox (
	id         TEXT PRIMARY KEY NOT NULL,
	consumer   TEXT NOT NULL,
	payload    TEXT NOT NULL,
	attempts   INTEGER NOT NULL DEFAULT 0,
	due        TEXT NOT NULL,
	last_error TEXT NOT NULL DEFAULT '',
	enqueued   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%fZ', 'now'))
);
CREATE INDEX IF NOT EXISTS idx_outbox_due ON outbox (due);
`

// dueLayout sorts as text in time order (fixed width, UTC).
const dueLayout = "2006-01-02T15:04:05.000000000Z"

// Entry is one piece of outstanding work.
type Entry struct {
	ID       string
	Consumer string
	// Payload is what the handler needs, as it last left it.
	Payload   json.RawMessage
	Attempts  int
	Due       time.Time
	LastError string
}

// Store is an outbox file.
type Store struct {
	db *sql.DB
	// wake tells a waiting Worker there is new work (Enqueue, Worker.Nudge).
	wake chan struct{}
}

// Open opens (creating if necessary) the outbox at path.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("outbox: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("outbox: init schema: %w", err)
	}
	return &Store{db: db, wake: make(chan struct{}, 1)}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Enqueue records work for consumer, due at due. id is derived from what
// asked for it (the event and the consumer), so asking again is the same
// entry: the first one stands.
func (s *Store) Enqueue(ctx context.Context, id, consumer string, payload json.RawMessage, due time.Time) error {
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO outbox (id, consumer, payload, due) VALUES (?, ?, ?, ?)`,
		id, consumer, string(payload), due.UTC().Format(dueLayout)); err != nil {
		return fmt.Errorf("outbox: enqueue %s: %w", id, err)
	}
	s.nudge()
	return nil
}

func (s *Store) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Due lists up to limit entries due by now, earliest first.
func (s *Store) Due(ctx context.Context, now time.Time, limit int) ([]Entry, error) {
	return s.query(ctx, `SELECT id, consumer, payload, attempts, due, last_error FROM outbox WHERE due <= ? ORDER BY due, id LIMIT ?`,
		now.UTC().Format(dueLayout), limit)
}

// Pending lists every entry, earliest due first (for operators).
func (s *Store) Pending(ctx context.Context) ([]Entry, error) {
	return s.query(ctx, `SELECT id, consumer, payload, attempts, due, last_error FROM outbox ORDER BY due, id`)
}

func (s *Store) query(ctx context.Context, q string, args ...any) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("outbox: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var payload, due string
		if err := rows.Scan(&e.ID, &e.Consumer, &payload, &e.Attempts, &due, &e.LastError); err != nil {
			return nil, fmt.Errorf("outbox: %w", err)
		}
		e.Payload = json.RawMessage(payload)
		if e.Due, err = time.Parse(dueLayout, due); err != nil {
			return nil, fmt.Errorf("outbox: entry %s due %q: %w", e.ID, due, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// NextDue is the earliest due time among consumers' entries (every
// consumer's when none is named), if there is any.
func (s *Store) NextDue(ctx context.Context, consumers ...string) (time.Time, bool, error) {
	q, args := `SELECT due FROM outbox`, []any{}
	if len(consumers) > 0 {
		q += ` WHERE consumer IN (?` + strings.Repeat(`, ?`, len(consumers)-1) + `)`
		for _, c := range consumers {
			args = append(args, c)
		}
	}
	var due string
	err := s.db.QueryRowContext(ctx, q+` ORDER BY due LIMIT 1`, args...).Scan(&due)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("outbox: %w", err)
	}
	t, err := time.Parse(dueLayout, due)
	return t, err == nil, err
}

// Reschedule records a failed attempt: the entry is due again at due, with
// payload (its handler's state, unchanged if nil) and the error.
func (s *Store) Reschedule(ctx context.Context, id string, payload json.RawMessage, attempts int, due time.Time, lastErr string) error {
	q := `UPDATE outbox SET attempts = ?, due = ?, last_error = ?`
	args := []any{attempts, due.UTC().Format(dueLayout), lastErr}
	if payload != nil {
		q += `, payload = ?`
		args = append(args, string(payload))
	}
	_, err := s.db.ExecContext(ctx, q+` WHERE id = ?`, append(args, id)...)
	return err
}

// Done takes an entry off.
func (s *Store) Done(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM outbox WHERE id = ?`, id)
	return err
}
