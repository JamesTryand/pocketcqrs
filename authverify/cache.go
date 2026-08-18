package authverify

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

// timeLayout is the fixed-width UTC format rows store, so string
// comparison in SQL orders the same way the times do (the idempotency
// store's convention).
const timeLayout = "2006-01-02T15:04:05.000Z"

const cacheSchema = `
CREATE TABLE IF NOT EXISTS auth_verdicts (
	token_hash      TEXT PRIMARY KEY,
	collection_name TEXT NOT NULL,
	collection_id   TEXT NOT NULL,
	record_json     TEXT NOT NULL,
	token_exp       TEXT NOT NULL,
	verified_at     TEXT NOT NULL,
	expires_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_auth_verdicts_expires ON auth_verdicts (expires_at);
`

// Cache persists verification verdicts in a small SQLite file of its own,
// off every hot path — the same shape as idempotency.Store, and SQLite
// rather than a map for the same reason C′ was chosen at all: a verdict
// cached before a master outage must survive a secondary restarting during
// it. The raw token is never stored; rows are keyed by HashToken.
type Cache struct {
	db *sql.DB
}

// OpenCache opens (creating if necessary) the verdict cache at path.
func OpenCache(path string) (*Cache, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("authverify: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(cacheSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("authverify: init schema: %w", err)
	}
	return &Cache{db: db}, nil
}

// Close closes the underlying database.
func (c *Cache) Close() error { return c.db.Close() }

// HashToken derives the cache key for a token. SHA-256 so a copy of the
// cache file never yields a usable credential.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Entry is one cached verdict.
type Entry struct {
	Verdict    Verdict
	TokenExp   time.Time // the token's own exp claim — no trust ever extends past it
	VerifiedAt time.Time
	ExpiresAt  time.Time // min(VerifiedAt+TTL, TokenExp), computed at save
}

// Lookup returns the cached entry for tokenHash, or (nil, nil) when there
// is none. Expiry is the caller's judgment: the Verifier also serves
// entries past ExpiresAt inside an operator-granted grace window, so
// Lookup does not filter them out.
func (c *Cache) Lookup(ctx context.Context, tokenHash string) (*Entry, error) {
	var collectionName, collectionID, recordJSON string
	var tokenExp, verifiedAt, expiresAt string
	err := c.db.QueryRowContext(ctx,
		`SELECT collection_name, collection_id, record_json, token_exp, verified_at, expires_at
		 FROM auth_verdicts WHERE token_hash = ?`, tokenHash,
	).Scan(&collectionName, &collectionID, &recordJSON, &tokenExp, &verifiedAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	entry := &Entry{Verdict: Verdict{
		CollectionName: collectionName,
		CollectionID:   collectionID,
		Record:         json.RawMessage(recordJSON),
	}}
	for dst, src := range map[*time.Time]string{
		&entry.TokenExp: tokenExp, &entry.VerifiedAt: verifiedAt, &entry.ExpiresAt: expiresAt,
	} {
		t, err := time.Parse(timeLayout, src)
		if err != nil {
			return nil, fmt.Errorf("authverify: corrupt cached timestamp %q: %w", src, err)
		}
		*dst = t
	}
	return entry, nil
}

// Save upserts a verdict. expiresAt must already be capped at the token's
// own exp — Verifier.expiry is the single place that computes it.
func (c *Cache) Save(ctx context.Context, tokenHash string, v *Verdict, tokenExp, verifiedAt, expiresAt time.Time) error {
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO auth_verdicts
			(token_hash, collection_name, collection_id, record_json, token_exp, verified_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (token_hash) DO UPDATE SET
			collection_name = excluded.collection_name,
			collection_id   = excluded.collection_id,
			record_json     = excluded.record_json,
			token_exp       = excluded.token_exp,
			verified_at     = excluded.verified_at,
			expires_at      = excluded.expires_at`,
		tokenHash, v.CollectionName, v.CollectionID, string(v.Record),
		tokenExp.UTC().Format(timeLayout), verifiedAt.UTC().Format(timeLayout),
		expiresAt.UTC().Format(timeLayout))
	return err
}

// Delete removes a verdict — a definitive rejection from the master must
// evict whatever was cached, not merely age out.
func (c *Cache) Delete(ctx context.Context, tokenHash string) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM auth_verdicts WHERE token_hash = ?`, tokenHash)
	return err
}

// Prune deletes entries no read can serve any more: past expiry by more
// than grace (grace 0 prunes at expiry exactly).
func (c *Cache) Prune(ctx context.Context, grace time.Duration) error {
	cutoff := time.Now().Add(-grace).UTC().Format(timeLayout)
	_, err := c.db.ExecContext(ctx, `DELETE FROM auth_verdicts WHERE expires_at < ?`, cutoff)
	return err
}

// StartPruner runs Prune on a fixed interval until ctx is done. logger may
// be nil (defaults to no-op).
func (c *Cache) StartPruner(ctx context.Context, interval, grace time.Duration, logger func(msg string, args ...any)) {
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
				if err := c.Prune(ctx, grace); err != nil {
					logger("authverify prune error", "error", err)
				}
			}
		}
	}()
}
