// Package idempotency lets the command gateway recognize a retried command
// and replay its original outcome instead of re-deciding it — the fix for
// F-4 (a client that retries after an ambiguous timeout can double-apply a
// command).
//
// Records live in their own small SQLite file, deliberately separate from
// events.db: the same reasoning behind PocketBase's own data/auxiliary
// split, and behind keeping any future command-intake queue off events.db's
// hot append path.
package idempotency

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// ErrKeyReused is returned by Lookup when key was previously saved against
// a different request (a different aggregate, id, command or body) than the
// one asking now. Replaying the old response would silently apply the
// wrong command; the caller must treat this as a client error.
var ErrKeyReused = errors.New("idempotency: key already used for a different request")

const schema = `
CREATE TABLE IF NOT EXISTS idempotency_keys (
	key           TEXT PRIMARY KEY,
	request_hash  TEXT NOT NULL,
	status        INTEGER NOT NULL,
	body          TEXT NOT NULL,
	created       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX IF NOT EXISTS idx_idempotency_created ON idempotency_keys (created);
`

// Store persists idempotency records for the command gateway.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the idempotency store at path.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("idempotency: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("idempotency: init schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Result is the stored outcome of a request previously saved under a key.
type Result struct {
	Status int
	Body   json.RawMessage
}

// Lookup reports the stored outcome for key, if any.
//
//   - No prior record: (nil, nil) — the caller should proceed and Save the
//     outcome when it has one.
//   - A prior record with the same requestHash: the stored Result, to
//     replay verbatim instead of re-deciding the command.
//   - A prior record with a different requestHash: (nil, ErrKeyReused).
func (s *Store) Lookup(ctx context.Context, key, requestHash string) (*Result, error) {
	var storedHash, body string
	var status int
	err := s.db.QueryRowContext(ctx,
		`SELECT request_hash, status, body FROM idempotency_keys WHERE key = ?`, key,
	).Scan(&storedHash, &status, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if storedHash != requestHash {
		return nil, ErrKeyReused
	}
	return &Result{Status: status, Body: json.RawMessage(body)}, nil
}

// Save records the outcome of the request identified by key and
// requestHash. If key already has a record (a racing duplicate request
// saved first), Save is a no-op: the first outcome wins, matching Lookup's
// replay contract.
func (s *Store) Save(ctx context.Context, key, requestHash string, status int, body json.RawMessage) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO idempotency_keys (key, request_hash, status, body) VALUES (?, ?, ?, ?)
		 ON CONFLICT (key) DO NOTHING`,
		key, requestHash, status, string(body))
	return err
}

// Prune deletes records older than maxAge.
func (s *Store) Prune(ctx context.Context, maxAge time.Duration) error {
	cutoff := time.Now().Add(-maxAge).UTC().Format("2006-01-02T15:04:05.000Z")
	_, err := s.db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE created < ?`, cutoff)
	return err
}

// StartPruner runs Prune on a fixed interval until ctx is done. logger may
// be nil (defaults to no-op).
func (s *Store) StartPruner(ctx context.Context, interval, maxAge time.Duration, logger func(msg string, args ...any)) {
	if logger == nil {
		logger = func(string, ...any) {}
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.Prune(ctx, maxAge); err != nil {
					logger("idempotency prune error", "error", err)
				}
			}
		}
	}()
}

// Hash computes the request identity Lookup/Save compare against, so the
// same key reused for a genuinely different request is caught rather than
// silently replayed or silently accepted. Order matters; callers must hash
// the same parts in the same order on every call for the same logical
// request.
func Hash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
