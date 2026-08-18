package authverify

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func openTestCache(t *testing.T) *Cache {
	t.Helper()
	c, err := OpenCache(filepath.Join(t.TempDir(), "authverify.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func testVerdict() *Verdict {
	return &Verdict{
		CollectionName: "_superusers",
		CollectionID:   "pbc_123",
		Record:         json.RawMessage(`{"id":"rec1","email":"a@b.c"}`),
	}
}

func TestCacheRoundTrip(t *testing.T) {
	c := openTestCache(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	if entry, err := c.Lookup(ctx, "h1"); err != nil || entry != nil {
		t.Fatalf("expected a clean miss, got (%v, %v)", entry, err)
	}

	exp := now.Add(time.Hour)
	if err := c.Save(ctx, "h1", testVerdict(), exp, now, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	entry, err := c.Lookup(ctx, "h1")
	if err != nil || entry == nil {
		t.Fatalf("expected a hit, got (%v, %v)", entry, err)
	}
	if entry.Verdict.CollectionName != "_superusers" || string(entry.Verdict.Record) != `{"id":"rec1","email":"a@b.c"}` {
		t.Fatalf("verdict did not round-trip: %+v", entry.Verdict)
	}
	if !entry.TokenExp.Equal(exp) || !entry.ExpiresAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("timestamps did not round-trip: %+v", entry)
	}
}

func TestCacheSaveIsAnUpsert(t *testing.T) {
	c := openTestCache(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := c.Save(ctx, "h1", testVerdict(), now.Add(time.Hour), now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	later := now.Add(30 * time.Second)
	if err := c.Save(ctx, "h1", testVerdict(), now.Add(time.Hour), later, later.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	entry, err := c.Lookup(ctx, "h1")
	if err != nil || entry == nil {
		t.Fatal(err)
	}
	if !entry.ExpiresAt.Equal(later.Add(time.Minute).UTC().Truncate(time.Millisecond)) {
		t.Fatalf("expected the re-save to win, got expiry %v", entry.ExpiresAt)
	}
}

func TestCacheDelete(t *testing.T) {
	c := openTestCache(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := c.Save(ctx, "h1", testVerdict(), now.Add(time.Hour), now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	if entry, err := c.Lookup(ctx, "h1"); err != nil || entry != nil {
		t.Fatalf("expected the entry gone, got (%v, %v)", entry, err)
	}
}

func TestCachePruneRespectsGrace(t *testing.T) {
	c := openTestCache(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// expired 10m ago and expired 10s ago
	if err := c.Save(ctx, "old", testVerdict(), now.Add(time.Hour), now.Add(-time.Hour), now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(ctx, "fresh", testVerdict(), now.Add(time.Hour), now.Add(-time.Hour), now.Add(-10*time.Second)); err != nil {
		t.Fatal(err)
	}

	// a 5m grace keeps the 10s-expired row (still servable during an
	// outage) and drops the 10m-expired one
	if err := c.Prune(ctx, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if entry, _ := c.Lookup(ctx, "old"); entry != nil {
		t.Fatal("expected the long-expired entry pruned")
	}
	if entry, _ := c.Lookup(ctx, "fresh"); entry == nil {
		t.Fatal("expected the within-grace entry kept")
	}
}

func TestCacheNeverStoresTheRawToken(t *testing.T) {
	c := openTestCache(t)
	ctx := context.Background()
	now := time.Now().UTC()

	token := "eyJhbGciOiJIUzI1NiJ9.a-very-recognizable-token-value.sig"
	if err := c.Save(ctx, HashToken(token), testVerdict(), now.Add(time.Hour), now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	rows, err := c.db.QueryContext(ctx, `SELECT token_hash FROM auth_verdicts`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		if key == token {
			t.Fatal("raw token persisted as a cache key")
		}
		if len(key) != 64 {
			t.Fatalf("expected a sha256 hex key, got %q", key)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
