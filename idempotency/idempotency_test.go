package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "idempotency.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestLookupMissIsNilNil(t *testing.T) {
	s := openTest(t)
	res, err := s.Lookup(context.Background(), "key-1", "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if res != nil {
		t.Fatalf("expected no record, got %+v", res)
	}
}

func TestSaveThenLookupReplaysSameRequest(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	body := json.RawMessage(`{"events":[{"type":"TaskCreated"}]}`)
	if err := s.Save(ctx, "key-1", "hash-1", 200, body); err != nil {
		t.Fatal(err)
	}

	res, err := s.Lookup(ctx, "key-1", "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("expected a stored result")
	}
	if res.Status != 200 || string(res.Body) != string(body) {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestLookupSameKeyDifferentRequestIsRejected(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if err := s.Save(ctx, "key-1", "hash-1", 200, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}

	_, err := s.Lookup(ctx, "key-1", "hash-2")
	if !errors.Is(err, ErrKeyReused) {
		t.Fatalf("expected ErrKeyReused, got %v", err)
	}
}

func TestSaveIsFirstWriteWins(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if err := s.Save(ctx, "key-1", "hash-1", 200, json.RawMessage(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	// a racing duplicate save under the same key must not overwrite
	if err := s.Save(ctx, "key-1", "hash-1", 409, json.RawMessage(`{"n":2}`)); err != nil {
		t.Fatal(err)
	}

	res, err := s.Lookup(ctx, "key-1", "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 || string(res.Body) != `{"n":1}` {
		t.Fatalf("expected the first save to win, got %+v", res)
	}
}

func TestPruneRemovesOnlyOldRecords(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if err := s.Save(ctx, "old", "hash", 200, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	// backdate it directly, past Save's own "now" default
	if _, err := s.db.ExecContext(ctx,
		`UPDATE idempotency_keys SET created = ? WHERE key = ?`,
		time.Now().Add(-48*time.Hour).UTC().Format("2006-01-02T15:04:05.000Z"), "old"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "fresh", "hash", 200, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}

	if err := s.Prune(ctx, 24*time.Hour); err != nil {
		t.Fatal(err)
	}

	if res, err := s.Lookup(ctx, "old", "hash"); err != nil {
		t.Fatal(err)
	} else if res != nil {
		t.Fatal("expected the old record to be pruned")
	}
	if res, err := s.Lookup(ctx, "fresh", "hash"); err != nil {
		t.Fatal(err)
	} else if res == nil {
		t.Fatal("expected the fresh record to survive")
	}
}

func TestHashIsOrderSensitiveAndStable(t *testing.T) {
	a := Hash("task", "t1", "Create", `{"title":"x"}`)
	b := Hash("task", "t1", "Create", `{"title":"x"}`)
	if a != b {
		t.Fatal("Hash must be deterministic for identical input")
	}
	c := Hash("task", "t1", "Create", `{"title":"y"}`)
	if a == c {
		t.Fatal("Hash must differ for a different body")
	}
	// concatenation ambiguity: "ab","c" must not hash the same as "a","bc"
	d := Hash("ab", "c")
	e := Hash("a", "bc")
	if d == e {
		t.Fatal("Hash must not be vulnerable to part-boundary concatenation collisions")
	}
}
