// Package events implements the PocketCQRS append-only event store.
//
// The store lives in its own SQLite file (events.db, next to PocketBase's
// data.db) and is the system's source of truth: appending events IS the
// commit. Per-aggregate sequences give optimistic concurrency; a global
// auto-increment position gives projections a total order to catch up from.
package events

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

// ErrConcurrency is returned by Append when the stream's current sequence
// does not match the caller's expected sequence.
var ErrConcurrency = errors.New("events: concurrency conflict")

// NewEvent is an event about to be appended.
type NewEvent struct {
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	// Version is the event schema version (0 is normalized to 1).
	// See the versioning contract: append-only property evolution,
	// transforms/upcasters between versions, new type for divergent shapes.
	Version int64 `json:"version,omitempty"`
}

// Event is a committed event envelope.
type Event struct {
	Position    int64           `json:"position"`
	ID          string          `json:"id"`
	Aggregate   string          `json:"aggregate"`
	AggregateID string          `json:"aggregateId"`
	Sequence    int64           `json:"sequence"`
	Type        string          `json:"type"`
	Data        json.RawMessage `json:"data"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Version     int64           `json:"version"`
	Created     string          `json:"created"`
}

const schema = `
CREATE TABLE IF NOT EXISTS events (
	position     INTEGER PRIMARY KEY AUTOINCREMENT,
	id           TEXT NOT NULL UNIQUE,
	aggregate    TEXT NOT NULL,
	aggregate_id TEXT NOT NULL,
	sequence     INTEGER NOT NULL,
	type         TEXT NOT NULL,
	data         TEXT NOT NULL,
	metadata     TEXT NOT NULL DEFAULT '{}',
	created      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%fZ', 'now')),
	UNIQUE (aggregate, aggregate_id, sequence)
);
CREATE INDEX IF NOT EXISTS idx_events_stream ON events (aggregate, aggregate_id, sequence);

CREATE TABLE IF NOT EXISTS projection_checkpoints (
	name     TEXT PRIMARY KEY,
	position INTEGER NOT NULL DEFAULT 0
);
`

// Store is an append-only event log backed by a single SQLite file.
type Store struct {
	db *sql.DB

	// mu serializes appends so the expected-sequence check and the insert
	// stay atomic from the store's point of view.
	mu sync.Mutex

	subsMu sync.RWMutex
	subs   []func(Event)
}

// Open opens (creating if necessary) the event store at path.
//
// The store self-migrates (tracked with PRAGMA user_version):
// v1 adds the version column to pre-versioning databases.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("events: open %s: %w", path, err)
	}
	// a single writer connection keeps SQLite happiest
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("events: init schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("events: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate applies store schema migrations idempotently.
func migrate(db *sql.DB) error {
	// v1: version column (absent in pre-versioning databases)
	var hasVersion int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('events') WHERE name = 'version'`).Scan(&hasVersion); err != nil {
		return err
	}
	if hasVersion == 0 {
		if _, err := db.Exec(`ALTER TABLE events ADD COLUMN version INTEGER NOT NULL DEFAULT 1`); err != nil {
			return err
		}
	}
	_, err := db.Exec(`PRAGMA user_version = 1`)
	return err
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Append atomically validates expectedSequence against the stream's current
// length and appends evts. Sequences are 1-based and contiguous, so the
// expected sequence equals the number of events already in the stream.
func (s *Store) Append(ctx context.Context, aggregate, aggregateID string, expectedSequence int64, evts []NewEvent) ([]Event, error) {
	if len(evts) == 0 {
		return nil, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var current int64
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sequence), 0) FROM events WHERE aggregate = ? AND aggregate_id = ?`,
		aggregate, aggregateID,
	).Scan(&current)
	if err != nil {
		return nil, err
	}
	if current != expectedSequence {
		return nil, fmt.Errorf("%w: stream %s/%s is at sequence %d, expected %d",
			ErrConcurrency, aggregate, aggregateID, current, expectedSequence)
	}

	appended := make([]Event, 0, len(evts))
	for i, ne := range evts {
		meta := ne.Metadata
		if len(meta) == 0 {
			meta = json.RawMessage(`{}`)
		}
		ev := Event{
			ID:          newID(),
			Aggregate:   aggregate,
			AggregateID: aggregateID,
			Sequence:    current + int64(i) + 1,
			Type:        ne.Type,
			Data:        ne.Data,
			Metadata:    meta,
			Version:     ne.Version,
		}
		if ev.Version <= 0 {
			ev.Version = 1
		}
		err = tx.QueryRowContext(ctx,
			`INSERT INTO events (id, aggregate, aggregate_id, sequence, type, data, metadata, version)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING position, created`,
			ev.ID, ev.Aggregate, ev.AggregateID, ev.Sequence, ev.Type, string(ev.Data), string(ev.Metadata), ev.Version,
		).Scan(&ev.Position, &ev.Created)
		if err != nil {
			return nil, err
		}
		appended = append(appended, ev)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	for _, ev := range appended {
		s.publish(ev)
	}
	return appended, nil
}

// LoadStream returns all events of one stream in sequence order.
func (s *Store) LoadStream(ctx context.Context, aggregate, aggregateID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT position, id, aggregate, aggregate_id, sequence, type, data, metadata, version, created
		 FROM events WHERE aggregate = ? AND aggregate_id = ? ORDER BY sequence`,
		aggregate, aggregateID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// ListStreams returns the ids of all streams of an aggregate
// (used by boot-time decider validation).
func (s *Store) ListStreams(ctx context.Context, aggregate string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT aggregate_id FROM events WHERE aggregate = ? ORDER BY aggregate_id`, aggregate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Poll returns up to limit events with position > after, in position order.
func (s *Store) Poll(ctx context.Context, after int64, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT position, id, aggregate, aggregate_id, sequence, type, data, metadata, version, created
		 FROM events WHERE position > ? ORDER BY position LIMIT ?`,
		after, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// Subscribe registers fn to be called (best-effort, in-process) with each
// event right after it commits. Consumers needing guaranteed delivery should
// poll via Poll with a durable checkpoint instead.
func (s *Store) Subscribe(fn func(Event)) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	s.subs = append(s.subs, fn)
}

func (s *Store) publish(ev Event) {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()
	for _, fn := range s.subs {
		fn := fn
		go func() {
			defer func() { _ = recover() }()
			fn(ev)
		}()
	}
}

// Checkpoint returns the durable position of a named consumer (0 if none).
func (s *Store) Checkpoint(ctx context.Context, name string) (int64, error) {
	var pos int64
	err := s.db.QueryRowContext(ctx,
		`SELECT position FROM projection_checkpoints WHERE name = ?`, name).Scan(&pos)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return pos, err
}

// SaveCheckpoint durably stores the position of a named consumer.
func (s *Store) SaveCheckpoint(ctx context.Context, name string, position int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO projection_checkpoints (name, position) VALUES (?, ?)
		 ON CONFLICT (name) DO UPDATE SET position = excluded.position`,
		name, position)
	return err
}

func scanEvents(rows *sql.Rows) ([]Event, error) {
	var out []Event
	for rows.Next() {
		var ev Event
		var data, meta string
		if err := rows.Scan(&ev.Position, &ev.ID, &ev.Aggregate, &ev.AggregateID,
			&ev.Sequence, &ev.Type, &data, &meta, &ev.Version, &ev.Created); err != nil {
			return nil, err
		}
		ev.Data = json.RawMessage(data)
		ev.Metadata = json.RawMessage(meta)
		out = append(out, ev)
	}
	return out, rows.Err()
}

func newID() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
