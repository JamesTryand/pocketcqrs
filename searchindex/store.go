// Package searchindex is search as a special kind of read model: the
// indexes behind readModel match filters (schema 3.1.0), the port of
// dotnetcqrs's search stores and index consumers. The pinned rules
// (notebook platform/eventmodeling-codegen findings.md, "D5 decisions" and
// "D6 decisions"):
//
//   - Every index lives in its own SQLite file (search.db), with
//     secure_delete on, excluded from backups and rebuilt from the log. Its
//     checkpoints live in the same file, so losing the file loses them too
//     and the indexes rebuild from zero rather than stay silently partial.
//   - Personal data: exact/prefix are blind indexes (the facade's keyed HMAC
//     of the normalized value; prefix stores one per prefix of at least
//     minPrefixLength code points, a search hashes the whole term); contains
//     keeps normalized plaintext, the one readable-at-rest form, which is why
//     the file is backup-excluded. A subject's rows are deleted on
//     SubjectErased and by the startup purge of the facade's erasure ledger;
//     a value whose key is destroyed is never indexed.
//   - Hashed indexes record the key version they were built with and always
//     pass it explicitly. A rotation is noticed at startup only: a second
//     consumer rebuilds at the new version while searches stay on the old,
//     and switches over once it catches up.
//
// Go-port difference, deliberate: dotnetcqrs answers a non-pii match from a
// normalized shadow column in the read model itself. Here read models are
// PocketBase collections whose schema comes from hand-written migrations, so
// non-pii match indexes live in search.db too, under the same rules.
package searchindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	_ "modernc.org/sqlite"

	"github.com/jamestryand/pocketcqrs/kms"
)

// ErasureLedgerCheckpoint is the store's cursor into the facade's erasure
// ledger.
const ErasureLedgerCheckpoint = "kms:erasure-ledger"

var tableName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// Store is search.db.
type Store struct {
	db *sql.DB

	mu     sync.Mutex
	tables map[string]bool
	active map[string]int // hashed index -> the key version searches use
}

// Open opens (creating) the index file at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=10000",
		"PRAGMA secure_delete=ON", // deleted rows are overwritten, not left in free pages
		`CREATE TABLE IF NOT EXISTS search_checkpoints (name TEXT PRIMARY KEY, position INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS search_index_versions (name TEXT PRIMARY KEY, version INTEGER NOT NULL)`,
	} {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("searchindex: %s: %w", p, err)
		}
	}
	return &Store{db: db, tables: map[string]bool{}, active: map[string]int{}}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// SecureDelete reports whether secure_delete is on (it must be).
func (s *Store) SecureDelete(ctx context.Context) (bool, error) {
	var on int
	err := s.db.QueryRowContext(ctx, "PRAGMA secure_delete").Scan(&on)
	return on == 1, err
}

// Checkpoint and SaveCheckpoint make the store a consumers.CheckpointStore,
// so an index's position lives beside the index.
func (s *Store) Checkpoint(ctx context.Context, name string) (int64, error) {
	var pos int64
	err := s.db.QueryRowContext(ctx, `SELECT position FROM search_checkpoints WHERE name = ?`, name).Scan(&pos)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return pos, err
}

func (s *Store) SaveCheckpoint(ctx context.Context, name string, position int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO search_checkpoints (name, position) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET position = excluded.position`, name, position)
	return err
}

func (s *Store) deleteCheckpoint(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM search_checkpoints WHERE name = ?`, name)
	return err
}

func (s *Store) indexVersion(ctx context.Context, name string) (int, bool, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT version FROM search_index_versions WHERE name = ?`, name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return v, err == nil, err
}

func (s *Store) setIndexVersion(ctx context.Context, name string, v int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO search_index_versions (name, version) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET version = excluded.version`, name, v)
	return err
}

// ActiveVersion is the key version searches of a hashed index use.
func (s *Store) ActiveVersion(index string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.active[index]
	return v, ok
}

func (s *Store) setActive(index string, v int) {
	s.mu.Lock()
	s.active[index] = v
	s.mu.Unlock()
}

// ensureTable creates one index table: (row_key, key_version, term, subject).
func (s *Store) ensureTable(ctx context.Context, table string) error {
	if !tableName.MatchString(table) {
		return fmt.Errorf("searchindex: bad table name %q", table)
	}
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %q (row_key TEXT NOT NULL, key_version INTEGER NOT NULL DEFAULT 0,
			term TEXT NOT NULL, subject TEXT NOT NULL DEFAULT '')`, table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %q (term)`, table+"_term", table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %q (row_key, key_version)`, table+"_row", table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %q (subject)`, table+"_subject", table),
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.tables[table] = true
	s.mu.Unlock()
	return nil
}

type row struct{ term, subject string }

// replace swaps one record's rows at one key version for rows, in one
// transaction, so a failed hash call (made before this) changes nothing.
func (s *Store) replace(ctx context.Context, table, rowKey string, version int, rows []row) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %q WHERE row_key = ? AND key_version = ?`, table), rowKey, version); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %q (row_key, key_version, term, subject) VALUES (?, ?, ?, ?)`, table),
			rowKey, version, r.term, r.subject); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) deleteOtherVersions(ctx context.Context, tables []string, keep ...int) error {
	marks := strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",")
	args := make([]any, len(keep))
	for i, k := range keep {
		args[i] = k
	}
	for _, t := range tables {
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %q WHERE key_version NOT IN (%s)`, t, marks), args...); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) truncate(ctx context.Context, tables []string) error {
	for _, t := range tables {
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %q`, t)); err != nil {
			return err
		}
	}
	return nil
}

// DeleteSubjects removes every row of the given subjects from every index
// table this store has opened. Returns the number of rows deleted.
func (s *Store) DeleteSubjects(ctx context.Context, subjects []string) (int64, error) {
	if len(subjects) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	tables := make([]string, 0, len(s.tables))
	for t := range s.tables {
		tables = append(tables, t)
	}
	s.mu.Unlock()
	var n int64
	for start := 0; start < len(subjects); start += 500 {
		chunk := subjects[start:min(start+500, len(subjects))]
		marks := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, len(chunk))
		for i, c := range chunk {
			args[i] = c
		}
		for _, t := range tables {
			res, err := s.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %q WHERE subject IN (%s)`, t, marks), args...)
			if err != nil {
				return n, err
			}
			k, _ := res.RowsAffected()
			n += k
		}
	}
	return n, nil
}

// Lookup returns the row keys whose term matches, at one key version:
// op "=" compares exactly, "LIKE" takes an already-escaped pattern. At most
// limit+1 keys come back, so a caller can tell "more than limit".
func (s *Store) Lookup(ctx context.Context, table string, version int, op, term string, limit int) ([]string, error) {
	if !tableName.MatchString(table) {
		return nil, fmt.Errorf("searchindex: bad table name %q", table)
	}
	cond := "term = ?"
	if op == "LIKE" {
		cond = `term LIKE ? ESCAPE '` + string(rune(92)) + `'`
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT DISTINCT row_key FROM %q WHERE key_version = ? AND %s LIMIT ?`, table, cond),
		version, term, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// PurgeErased deletes every index row of every subject in the facade's
// erasure ledger since the last run. Run at startup, after the indexes'
// tables are ensured: it catches erasures this node missed while its index
// file was missing or restored. The cursor is saved only after a page's
// rows are gone, so an interrupted run repeats a page rather than skipping
// one. A ledger shorter than the cursor (rebuilt facade) restarts once; a
// facade that answers a non-empty page without advancing fails rather than
// loop forever.
func (s *Store) PurgeErased(ctx context.Context, client kms.Client) (int64, error) {
	cursor, err := s.Checkpoint(ctx, ErasureLedgerCheckpoint)
	if err != nil {
		return 0, err
	}
	var deleted int64
	restarted := false
	for {
		page, err := client.ListErasures(ctx, cursor, kms.MaxBatchItems)
		if err != nil {
			return deleted, err
		}
		if len(page.SubjectIDs) == 0 {
			if page.Next != cursor {
				if err := s.SaveCheckpoint(ctx, ErasureLedgerCheckpoint, page.Next); err != nil {
					return deleted, err
				}
			}
			return deleted, nil
		}
		if page.Next <= cursor {
			if restarted || page.Next == cursor {
				return deleted, &kms.ProtocolError{Msg: fmt.Sprintf("erasure ledger did not advance past %d", cursor)}
			}
			restarted = true
		}
		n, err := s.DeleteSubjects(ctx, page.SubjectIDs)
		if err != nil {
			return deleted, err
		}
		deleted += n
		if err := s.SaveCheckpoint(ctx, ErasureLedgerCheckpoint, page.Next); err != nil {
			return deleted, err
		}
		cursor = page.Next
	}
}
