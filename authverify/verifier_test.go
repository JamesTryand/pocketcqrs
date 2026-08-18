package authverify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/pocketbase/pocketbase/tools/security"
)

// mintToken makes a structurally valid auth token. The signature is over a
// throwaway key — only the master judges signatures, and these tests fake
// the master; the verifier itself must treat claims as untrusted apart
// from using exp to SHORTEN trust.
func mintToken(t *testing.T, ttl time.Duration) string {
	t.Helper()
	token, err := security.NewJWT(jwt.MapClaims{
		"id":           "rec1",
		"collectionId": "pbc_123",
		"type":         "auth",
	}, "test-signing-key", ttl)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// fakeMaster is a stand-in verify endpoint: answers status (200 with a
// verdict, or the code as-is) and counts calls.
type fakeMaster struct {
	srv    *httptest.Server
	status atomic.Int64
	calls  atomic.Int64
}

func newFakeMaster(t *testing.T) *fakeMaster {
	t.Helper()
	m := &fakeMaster{}
	m.status.Store(http.StatusOK)
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.calls.Add(1)
		status := int(m.status.Load())
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(testVerdict())
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *fakeMaster) url(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse(m.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func newTestVerifier(t *testing.T, master *url.URL, ttl, grace time.Duration) *Verifier {
	t.Helper()
	cache, err := OpenCache(filepath.Join(t.TempDir(), "authverify.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	return New(master, cache, ttl, grace)
}

func TestVerifyCachedCachesTheVerdict(t *testing.T) {
	master := newFakeMaster(t)
	v := newTestVerifier(t, master.url(t), 5*time.Minute, 0)
	token := mintToken(t, time.Hour)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		verdict, err := v.VerifyCached(ctx, token)
		if err != nil || verdict.CollectionName != "_superusers" {
			t.Fatalf("call %d: (%v, %v)", i, verdict, err)
		}
	}
	if got := master.calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 master round-trip for 3 checks, got %d", got)
	}
}

func TestVerifyCachedCapsExpiryAtTheTokensOwnExp(t *testing.T) {
	master := newFakeMaster(t)
	v := newTestVerifier(t, master.url(t), time.Hour, 0)
	token := mintToken(t, time.Minute) // token outlived by the TTL
	ctx := context.Background()

	if _, err := v.VerifyCached(ctx, token); err != nil {
		t.Fatal(err)
	}
	entry, err := v.cache.Lookup(ctx, HashToken(token))
	if err != nil || entry == nil {
		t.Fatal(err)
	}
	if entry.ExpiresAt.After(entry.TokenExp) {
		t.Fatalf("cached verdict outlives its token: expires %v, token exp %v",
			entry.ExpiresAt, entry.TokenExp)
	}
	if entry.ExpiresAt.After(time.Now().Add(2 * time.Minute)) {
		t.Fatalf("expected expiry near the token's 1m exp, not the 1h TTL: %v", entry.ExpiresAt)
	}
}

func TestVerifyCachedRejectsExpiredTokenWithoutAskingMaster(t *testing.T) {
	master := newFakeMaster(t)
	v := newTestVerifier(t, master.url(t), 5*time.Minute, 0)
	token := mintToken(t, -time.Minute)

	_, err := v.VerifyCached(context.Background(), token)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken, got %v", err)
	}
	if master.calls.Load() != 0 {
		t.Fatal("an already-expired token must not cost a master round-trip")
	}
}

func TestVerifyCachedDefinitiveRejectionEvicts(t *testing.T) {
	master := newFakeMaster(t)
	v := newTestVerifier(t, master.url(t), 5*time.Minute, 0)
	token := mintToken(t, time.Hour)
	ctx := context.Background()

	if _, err := v.VerifyCached(ctx, token); err != nil {
		t.Fatal(err)
	}
	// the verdict is now cached; simulate revocation at master and expire
	// the cache entry so the next check re-asks
	master.status.Store(http.StatusUnauthorized)
	v.now = func() time.Time { return time.Now().Add(10 * time.Minute) }

	_, err := v.VerifyCached(ctx, token)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken after revocation, got %v", err)
	}
	if entry, _ := v.cache.Lookup(ctx, HashToken(token)); entry != nil {
		t.Fatal("expected the rejected token's cache entry evicted")
	}
}

func TestVerifyCachedServesUnexpiredHitWithMasterDown(t *testing.T) {
	master := newFakeMaster(t)
	v := newTestVerifier(t, master.url(t), 5*time.Minute, 0)
	token := mintToken(t, time.Hour)
	ctx := context.Background()

	if _, err := v.VerifyCached(ctx, token); err != nil {
		t.Fatal(err)
	}
	master.srv.Close()

	verdict, err := v.VerifyCached(ctx, token)
	if err != nil || verdict == nil {
		t.Fatalf("expected the unexpired cached verdict to serve with master down, got %v", err)
	}
}

func TestVerifyCachedFailsClosedPastExpiryWithoutGrace(t *testing.T) {
	master := newFakeMaster(t)
	v := newTestVerifier(t, master.url(t), 5*time.Minute, 0)
	token := mintToken(t, time.Hour)
	ctx := context.Background()

	if _, err := v.VerifyCached(ctx, token); err != nil {
		t.Fatal(err)
	}
	master.srv.Close()
	v.now = func() time.Time { return time.Now().Add(10 * time.Minute) }

	_, err := v.VerifyCached(ctx, token)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("expected fail-closed ErrUnreachable, got %v", err)
	}
}

func TestVerifyCachedServesStaleWithinGrace(t *testing.T) {
	master := newFakeMaster(t)
	v := newTestVerifier(t, master.url(t), 5*time.Minute, 30*time.Minute)
	token := mintToken(t, time.Hour)
	ctx := context.Background()

	if _, err := v.VerifyCached(ctx, token); err != nil {
		t.Fatal(err)
	}
	master.srv.Close()

	// 10m in: past the 5m TTL, within TTL+30m grace, within the token's 1h exp
	v.now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	verdict, err := v.VerifyCached(ctx, token)
	if err != nil || verdict == nil {
		t.Fatalf("expected the stale verdict within grace, got %v", err)
	}

	// 2h in: grace can never extend past the token's own exp
	v.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := v.VerifyCached(ctx, token); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("expected fail-closed past the token's exp even within grace, got %v", err)
	}
}

func TestVerifyCachedFailsClosedOnMissWithMasterDown(t *testing.T) {
	master := newFakeMaster(t)
	master.srv.Close()
	v := newTestVerifier(t, master.url(t), 5*time.Minute, 30*time.Minute)

	_, err := v.VerifyCached(context.Background(), mintToken(t, time.Hour))
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("expected ErrUnreachable on a cold miss with master down, got %v", err)
	}
}

func TestVerifyFreshIgnoresTheCache(t *testing.T) {
	master := newFakeMaster(t)
	v := newTestVerifier(t, master.url(t), 5*time.Minute, 30*time.Minute)
	token := mintToken(t, time.Hour)
	ctx := context.Background()

	if _, err := v.VerifyCached(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := v.VerifyFresh(ctx, token); err != nil {
		t.Fatal(err)
	}
	if got := master.calls.Load(); got != 2 {
		t.Fatalf("expected VerifyFresh to round-trip despite a live cache entry, got %d calls", got)
	}

	// and no grace: master down means no verdict, cache or not
	master.srv.Close()
	if _, err := v.VerifyFresh(ctx, token); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("expected VerifyFresh to fail closed with master down, got %v", err)
	}
}
