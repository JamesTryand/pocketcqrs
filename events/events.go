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
	"sort"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// ErrConcurrency is returned by Append when the stream's current sequence
// does not match the caller's expected sequence.
var ErrConcurrency = errors.New("events: concurrency conflict")

// ErrReadOnly is returned by every write method (Append, SaveCheckpoint,
// SetMeta, SetMode) on a Store opened with OpenReadOnly.
var ErrReadOnly = errors.New("events: store is read-only")

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

CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// System modes (meta key "mode"): the barrier between serving traffic and
// schema-changing work (see the reload endpoint and `system maintenance`).
const (
	// MetaMode is the meta key holding the system mode.
	MetaMode = "mode"
	// ModeRunning is the normal serving mode (default when unset).
	ModeRunning = "running"
	// ModeMaintenance rejects domain commands and permits reloading
	// schema-bearing function files (projection schemas, deciders).
	ModeMaintenance = "maintenance"
)

// Store is an append-only event log backed by a single SQLite file.
type Store struct {
	db *sql.DB

	// readOnly marks a Store opened with OpenReadOnly: every write method
	// fails fast with ErrReadOnly instead of surfacing an opaque SQLite
	// error from a read-only connection (or, worse, a VFS that silently
	// discards the write).
	readOnly bool

	// mu serializes appends so the expected-sequence check and the insert
	// stay atomic from the store's point of view.
	mu sync.Mutex

	subsMu sync.RWMutex
	subs   []func(Event)

	// upcaster is the read-path event upcaster (see SetUpcaster); guarded
	// so it can be swapped (e.g. on function reload) while readers poll.
	upcastMu sync.RWMutex
	upcaster func(Event) (Event, error)

	// CommitBatchFault, if set, is called by CommitBatch immediately
	// before it would commit -- the exact point a real process crash
	// between validating a batch and durably writing it would land. A
	// non-nil return aborts the transaction (rolled back, nothing
	// written) and is returned as CommitBatch's error, standing in for
	// that crash without an actual process kill. For fault-injection
	// testing only -- never set in production code.
	CommitBatchFault func() error
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

// OpenOption configures OpenReadOnly.
type OpenOption func(*openConfig)

type openConfig struct {
	vfs string
}

// WithVFS opens through a SQLite VFS already registered with the driver
// under name (e.g. a Litestream-provided VFS mounting a replicated
// events.db), instead of the ordinary OS filesystem.
func WithVFS(name string) OpenOption {
	return func(c *openConfig) { c.vfs = name }
}

// OpenReadOnly opens the event store at path for reading only: unlike Open,
// it creates no schema, runs no migration, and returns a Store whose write
// methods (Append, SaveCheckpoint, SetMeta, SetMode) all fail with
// ErrReadOnly. path must already exist and be schema-current — the writer
// that owns it (Open) is responsible for creating and migrating it.
//
// This is the entry point a single-writer/multi-reader secondary uses: it
// polls a locally-replicated copy of the master's events.db and must never
// append to it directly. A secondary's own consumer checkpoints cannot live
// in this Store either, for the same reason — consumers.NewEngineWithCheckpoints
// takes a separate, locally writable CheckpointStore for exactly this case.
//
// Deliberately does not request journal_mode(WAL): a reader doesn't need to
// ask for the mode a file is already in -- SQLite negotiates it from the
// file header on open -- and requesting it is actively harmful, not just
// redundant, against a copy kept in sync by Litestream's `restore -f`
// (litestream-vfs-scope.md): that tool deliberately rewrites a followed
// copy's header into DELETE journal mode as part of its own concurrent-
// reader safety mechanism, and a mode=ro connection cannot honor a
// WAL-mode request against that file -- confirmed directly, not assumed:
// it fails outright with "attempt to write a readonly database (8)" rather
// than silently no-opping. Omitting the pragma is safe for both the
// already-verified same-host WAL case (plain-file --cqrsEventsPath) and
// this Litestream-followed DELETE-mode case, since the connection just
// reads whichever mode the file is actually in either way.
func OpenReadOnly(path string, opts ...OpenOption) (*Store, error) {
	var cfg openConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=query_only(1)&mode=ro",
		path)
	if cfg.vfs != "" {
		dsn += "&vfs=" + cfg.vfs
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("events: open read-only %s: %w", path, err)
	}
	// sql.Open never dials; without an eager check a missing file or
	// not-yet-migrated schema would surface only on the caller's first
	// Poll, not here.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("events: open read-only %s: %w", path, err)
	}
	// no SetMaxOpenConns(1): there is no writer to keep happy, and readers
	// benefit from concurrent connections in WAL mode.
	return &Store{db: db, readOnly: true}, nil
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

	// v2: dead_letters table
	if _, err := db.Exec(deadLettersSchema); err != nil {
		return err
	}

	// v3: meta kv table (system mode, future platform flags)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`); err != nil {
		return err
	}

	// v4: partial index on the commandId a batching commit stamps into
	// event metadata (see CommitBatch, CommandApplied) -- only indexes
	// rows that actually carry one, so cost is proportional to actual
	// batching use, not the whole table.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_events_command_id
		ON events (json_extract(metadata, '$.commandId'))
		WHERE json_extract(metadata, '$.commandId') IS NOT NULL`); err != nil {
		return err
	}

	_, err := db.Exec(`PRAGMA user_version = 4`)
	return err
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Append atomically validates expectedSequence against the stream's current
// length and appends evts. Sequences are 1-based and contiguous, so the
// expected sequence equals the number of events already in the stream.
func (s *Store) Append(ctx context.Context, aggregate, aggregateID string, expectedSequence int64, evts []NewEvent) ([]Event, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
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

// ImportEvents bulk-inserts previously-committed events (as produced by
// LoadStreamRaw / a pack's event-data export), preserving each event's own
// id/aggregate/aggregateId/sequence/type/data/metadata/version/created.
// Positions are freshly assigned by AUTOINCREMENT in the order given, so the
// batch lands at the tail of this store's own position sequence — never
// interleaved by original wall-clock time, which is not portable across
// stores.
//
// Refuses the WHOLE batch, before writing anything, on either kind of
// collision against this store's EXISTING data (not against other events in
// the same batch): an exact (aggregate, aggregateId) match — a real
// overwrite risk — or the batch naming any aggregate this store already has
// ANY stream of, even under a different id. The second check is taken
// literally from pack-portability-scope.md's "hard refusal... on any
// aggregate-name or aggregate+id collision" and is deliberately stricter
// than it may look: two merging systems sharing ordinary vocabulary (both
// have a "task" aggregate) hit it even with disjoint ids. See
// events-db-slice-merge-scope.md for the full rationale and the named rough
// edge (a future opt-out is the likely relief valve, not implemented here).
//
// Read-only stores refuse with ErrReadOnly, same as Append. An empty evts
// is a no-op, matching Append.
func (s *Store) ImportEvents(ctx context.Context, evts []Event) ([]Event, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
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

	if err := checkImportCollisions(ctx, tx, evts); err != nil {
		return nil, err
	}

	imported := make([]Event, 0, len(evts))
	for _, ev := range evts {
		out := ev
		if len(out.Metadata) == 0 {
			out.Metadata = json.RawMessage(`{}`)
		}
		if out.Version <= 0 {
			out.Version = 1
		}
		err = tx.QueryRowContext(ctx,
			`INSERT INTO events (id, aggregate, aggregate_id, sequence, type, data, metadata, version, created)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING position`,
			out.ID, out.Aggregate, out.AggregateID, out.Sequence, out.Type,
			string(out.Data), string(out.Metadata), out.Version, out.Created,
		).Scan(&out.Position)
		if err != nil {
			return nil, fmt.Errorf("events: import %s/%s#%d: %w", out.Aggregate, out.AggregateID, out.Sequence, err)
		}
		imported = append(imported, out)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	for _, ev := range imported {
		s.publish(ev)
	}
	return imported, nil
}

// CheckImportCollisions reports the same refusal ImportEvents(ctx, evts)
// would produce, without writing anything — for an "events import
// --dry-run" preview that needs to report a real collision outcome, not
// merely echo the input back. Runs the identical check ImportEvents itself
// uses (checkImportCollisions), just against s.db directly instead of an
// open transaction, since a preview takes no write lock. As with any
// preview, a write landing between this call and a real import can change
// the answer; the guarantee is on ImportEvents' own atomicity, not on this
// method never going stale.
func (s *Store) CheckImportCollisions(ctx context.Context, evts []Event) error {
	return checkImportCollisions(ctx, s.db, evts)
}

// queryRower is satisfied by both *sql.DB and *sql.Tx, so
// checkImportCollisions can run identically inside ImportEvents' own
// transaction (atomic with the insert) and standalone from
// CheckImportCollisions (a preview, no transaction needed).
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// checkImportCollisions refuses on either kind of collision against q's
// existing data (not against other events in the same batch): an exact
// (aggregate, aggregateId) match — a real overwrite risk — or the batch
// naming any aggregate q already has ANY stream of, even under a different
// id. The second check is taken literally from pack-portability-scope.md's
// "hard refusal... on any aggregate-name or aggregate+id collision" and is
// deliberately stricter than it may look: two merging systems sharing
// ordinary vocabulary (both have a "task" aggregate) hit it even with
// disjoint ids. See events-db-slice-merge-scope.md for the full rationale
// and the named rough edge (a future opt-out is the likely relief valve,
// not implemented here).
func checkImportCollisions(ctx context.Context, q queryRower, evts []Event) error {
	type streamKey struct{ aggregate, id string }
	streamKeys := map[streamKey]bool{}
	aggNames := map[string]bool{}
	for _, ev := range evts {
		streamKeys[streamKey{ev.Aggregate, ev.AggregateID}] = true
		aggNames[ev.Aggregate] = true
	}

	var collisions []string
	for k := range streamKeys {
		var n int
		if err := q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM events WHERE aggregate = ? AND aggregate_id = ?`,
			k.aggregate, k.id,
		).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			collisions = append(collisions, fmt.Sprintf("%s/%s: this store already has that exact stream", k.aggregate, k.id))
		}
	}
	for name := range aggNames {
		var n int
		if err := q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM events WHERE aggregate = ?`, name,
		).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			collisions = append(collisions, fmt.Sprintf("aggregate %q: this store already has stream(s) under this name", name))
		}
	}
	if len(collisions) > 0 {
		sort.Strings(collisions)
		return fmt.Errorf("events: import refused, %d collision(s):\n  %s",
			len(collisions), strings.Join(collisions, "\n  "))
	}
	return nil
}

// LoadStream returns all events of one stream in sequence order.
func (s *Store) LoadStream(ctx context.Context, aggregate, aggregateID string) ([]Event, error) {
	evs, err := s.loadStreamRows(ctx, aggregate, aggregateID)
	if err != nil {
		return nil, err
	}
	return s.upcastEvents(evs)
}

// LoadStreamRaw is LoadStream without the read-path upcaster: the stored
// rows exactly as committed, at whatever version they were written.
//
// Only export-for-portability (pack event-data export) should use this.
// Every in-process consumer — deciders, projections, functions, reactors —
// must keep going through the upcasted path (LoadStream/Poll/QueryEvents),
// so a running decider or projection never has to fold more than one schema
// shape. Exporting through the upcasted path instead would (a) break
// packs.md's "stored rows never rewritten" guarantee at the export boundary,
// since the exported file would hold upcast, not stored, data, and (b) on
// merge — where the target is an independently-built system with no
// guarantee of holding the source's transform chain — silently ship rows at
// a version nothing there can fold, discovered only on the next read of
// that stream.
func (s *Store) LoadStreamRaw(ctx context.Context, aggregate, aggregateID string) ([]Event, error) {
	return s.loadStreamRows(ctx, aggregate, aggregateID)
}

func (s *Store) loadStreamRows(ctx context.Context, aggregate, aggregateID string) ([]Event, error) {
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

// StreamInfo describes one stream: its length, latest global position and
// last-write timestamp (the operational stream listing behind
// GET /api/cqrs/streams).
type StreamInfo struct {
	Aggregate    string `json:"aggregate"`
	AggregateID  string `json:"aggregateId"`
	Events       int64  `json:"events"`
	LastPosition int64  `json:"lastPosition"`
	Updated      string `json:"updated"`
}

// ListStreamInfos returns one row per stream, ordered by aggregate and
// stream id. An empty aggregate lists streams of every aggregate type.
func (s *Store) ListStreamInfos(ctx context.Context, aggregate string) ([]StreamInfo, error) {
	query := `SELECT aggregate, aggregate_id, COUNT(*), MAX(position), MAX(created)
		 FROM events`
	args := []any{}
	if aggregate != "" {
		query += ` WHERE aggregate = ?`
		args = append(args, aggregate)
	}
	query += ` GROUP BY aggregate, aggregate_id ORDER BY aggregate, aggregate_id`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StreamInfo
	for rows.Next() {
		var si StreamInfo
		if err := rows.Scan(&si.Aggregate, &si.AggregateID, &si.Events, &si.LastPosition, &si.Updated); err != nil {
			return nil, err
		}
		out = append(out, si)
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
	evs, err := scanEvents(rows)
	if err != nil {
		return nil, err
	}
	return s.upcastEvents(evs)
}

// EventQuery filters a log read (the operational feed behind
// GET /api/cqrs/events and out-of-process consumers).
type EventQuery struct {
	// After returns events with position > After (0 = from the start).
	After int64
	// Before returns events with position < Before (0 = no upper bound).
	// It also flips the scan: the batch is taken from the Before end, so a
	// caller can page backwards. See QueryEvents for what that means when
	// both bounds are set.
	Before int64
	// Limit caps the batch (<= 0 defaults to 100).
	Limit int
	// Aggregate restricts to one aggregate type ("" = all).
	Aggregate string
	// AggregateID restricts to one stream ("" = all). Pair it with
	// Aggregate: stream identity is (aggregate, aggregateId).
	AggregateID string
	// Type restricts to one event type ("" = all).
	Type string
}

// QueryEvents returns the events matching q, always in ascending position
// order. Like Poll, results pass through the installed upcaster: feed
// consumers see events at their latest schema version.
//
// After and Before compose as bounds, but only Before decides which end the
// Limit is taken from. With Before set the scan runs descending and the
// batch is the Limit events immediately BELOW Before — After is then a floor
// guard, not the start of the window. (Paging backwards cannot be expressed
// as "after − limit": under a filter the matching positions are sparse.)
func (s *Store) QueryEvents(ctx context.Context, q EventQuery) ([]Event, error) {
	if q.Limit <= 0 {
		q.Limit = 100
	}
	query := `SELECT position, id, aggregate, aggregate_id, sequence, type, data, metadata, version, created
		 FROM events WHERE position > ?`
	args := []any{q.After}
	if q.Before > 0 {
		query += ` AND position < ?`
		args = append(args, q.Before)
	}
	if q.Aggregate != "" {
		query += ` AND aggregate = ?`
		args = append(args, q.Aggregate)
	}
	if q.AggregateID != "" {
		query += ` AND aggregate_id = ?`
		args = append(args, q.AggregateID)
	}
	if q.Type != "" {
		query += ` AND type = ?`
		args = append(args, q.Type)
	}
	if q.Before > 0 {
		query += ` ORDER BY position DESC LIMIT ?`
	} else {
		query += ` ORDER BY position LIMIT ?`
	}
	args = append(args, q.Limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	evs, err := scanEvents(rows)
	if err != nil {
		return nil, err
	}
	if q.Before > 0 {
		// scanned newest-first; hand back ascending like every other read
		for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
			evs[i], evs[j] = evs[j], evs[i]
		}
	}
	return s.upcastEvents(evs)
}

// SetUpcaster installs the read-path upcaster: every event handed out by
// LoadStream, Poll and QueryEvents is passed through it, so all consumers —
// deciders, projections, functions, reactors, feed readers — see events at
// their latest schema version. The stored row is never rewritten: upcasting is a read-time
// view, consistent with the append-only log. A nil upcaster disables it.
func (s *Store) SetUpcaster(fn func(Event) (Event, error)) {
	s.upcastMu.Lock()
	defer s.upcastMu.Unlock()
	s.upcaster = fn
}

// upcastEvents passes a freshly scanned batch through the installed
// upcaster (if any). A failure fails the whole read: consumers must never
// see an event in a shape its transform chain could not produce.
func (s *Store) upcastEvents(evs []Event) ([]Event, error) {
	s.upcastMu.RLock()
	upcaster := s.upcaster
	s.upcastMu.RUnlock()
	if upcaster == nil {
		return evs, nil
	}
	for i := range evs {
		up, err := upcaster(evs[i])
		if err != nil {
			return nil, fmt.Errorf("events: upcast %s/%s#%d (%s v%d): %w",
				evs[i].Aggregate, evs[i].AggregateID, evs[i].Sequence, evs[i].Type, evs[i].Version, err)
		}
		evs[i] = up
	}
	return evs, nil
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
	if s.readOnly {
		return ErrReadOnly
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO projection_checkpoints (name, position) VALUES (?, ?)
		 ON CONFLICT (name) DO UPDATE SET position = excluded.position`,
		name, position)
	return err
}

// GetMeta returns the meta value for key ("" when unset).
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta upserts a meta value.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

// Mode returns the system mode (ModeRunning when never set).
func (s *Store) Mode(ctx context.Context) (string, error) {
	mode, err := s.GetMeta(ctx, MetaMode)
	if err != nil || mode == "" {
		return ModeRunning, err
	}
	return mode, nil
}

// SetMode persists the system mode (ModeRunning or ModeMaintenance).
func (s *Store) SetMode(ctx context.Context, mode string) error {
	if mode != ModeRunning && mode != ModeMaintenance {
		return fmt.Errorf("events: invalid system mode %q (want %s or %s)", mode, ModeRunning, ModeMaintenance)
	}
	return s.SetMeta(ctx, MetaMode, mode)
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
