package authverify

import (
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

// twoNodes stands up the F-13 topology in miniature: a master with a
// superuser record and the verify endpoint, and a secondary cloned from
// the same fixture — same collections, NOT that record — running the
// verifying middleware, a route gated the way PocketBase routes are
// (apis.RequireSuperuserAuth), and one gated the way the ops routes are
// (RequireSuperuser, fresh).
func twoNodes(t *testing.T, ttl, grace time.Duration) (masterURL string, secondary string, rec *core.Record, token string, v *Verifier, masterApp core.App) {
	t.Helper()

	mApp := openTestApp(t)
	rec, token = newSuperuser(t, mApp, "f13-mw@example.com")
	master := serveNode(t, mApp, func(e *core.ServeEvent) { RegisterEndpoint(e, nil) })

	base, err := url.Parse(master.URL)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := OpenCache(filepath.Join(t.TempDir(), "authverify.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	verifier := New(base, cache, ttl, grace)

	sApp := openTestApp(t)
	sec := serveNode(t, sApp, func(e *core.ServeEvent) {
		Register(e, verifier)
		e.Router.GET("/f13-protected", func(re *core.RequestEvent) error {
			return re.JSON(http.StatusOK, map[string]string{"as": re.Auth.Id})
		}).Bind(apis.RequireSuperuserAuth())
		e.Router.GET("/f13-ops", func(re *core.RequestEvent) error {
			return re.JSON(http.StatusOK, map[string]string{"as": re.Auth.Id})
		}).Bind(RequireSuperuser(verifier))
	})

	return master.URL, sec.URL, rec, token, verifier, mApp
}

func get(t *testing.T, rawURL, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestMiddlewareAuthenticatesAMasterMintedTokenLocally(t *testing.T) {
	_, sec, _, token, _, _ := twoNodes(t, 5*time.Minute, 0)

	// the exact F-13 failure: this route's gate needs re.Auth, the token is
	// master-minted, and this node's data.db has neither the secret nor the
	// record — before this package, an unconditional 401
	if resp := get(t, sec+"/f13-protected", token); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the master-minted token to authenticate locally, got %d", resp.StatusCode)
	}
	// unauthenticated and garbage still answer 401, not 503: only "master
	// unreachable" may read as an outage
	if resp := get(t, sec+"/f13-protected", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a token, got %d", resp.StatusCode)
	}
	if resp := get(t, sec+"/f13-protected", "garbage"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a garbage token, got %d", resp.StatusCode)
	}
}

func TestOpsGateRevocationIsImmediateWhileCachedRoutesLag(t *testing.T) {
	_, sec, rec, token, _, masterApp := twoNodes(t, 5*time.Minute, 0)

	// prime both paths
	if resp := get(t, sec+"/f13-protected", token); resp.StatusCode != http.StatusOK {
		t.Fatalf("prime cached route: %d", resp.StatusCode)
	}
	if resp := get(t, sec+"/f13-ops", token); resp.StatusCode != http.StatusOK {
		t.Fatalf("prime ops route: %d", resp.StatusCode)
	}

	// revoke at master: PocketBase's own per-user logout mechanism
	rec.RefreshTokenKey()
	if err := masterApp.Save(rec); err != nil {
		t.Fatal(err)
	}

	// the cached end-user route still serves within its TTL — C′'s stated,
	// bounded revocation lag, demonstrated rather than assumed. (Checked
	// BEFORE the ops route: a fresh rejection there evicts the cache entry.)
	if resp := get(t, sec+"/f13-protected", token); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the cached route to still serve within TTL, got %d", resp.StatusCode)
	}
	// the ops gate re-verifies fresh — revocation bites immediately
	if resp := get(t, sec+"/f13-ops", token); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected the ops gate to reject a revoked token immediately, got %d", resp.StatusCode)
	}
	// and that fresh rejection evicted the cached verdict, so even the
	// cached route now rejects — revocation propagates the moment any
	// fresh check sees it, not only at TTL expiry
	if resp := get(t, sec+"/f13-protected", token); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected the eviction to reach the cached route too, got %d", resp.StatusCode)
	}
}

func TestMiddlewareFailsClosedAs503WhenMasterIsDownCold(t *testing.T) {
	master := newFakeMaster(t)
	master.srv.Close()
	base, _ := url.Parse(master.srv.URL)
	cache, err := OpenCache(filepath.Join(t.TempDir(), "authverify.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	verifier := New(base, cache, 5*time.Minute, 0)

	sApp := openTestApp(t)
	sec := serveNode(t, sApp, func(e *core.ServeEvent) {
		Register(e, verifier)
		e.Router.GET("/f13-protected", func(re *core.RequestEvent) error {
			return re.JSON(http.StatusOK, map[string]string{"ok": "1"})
		}).Bind(apis.RequireSuperuserAuth())
	})

	// a plausible token, no cached verdict, master gone: 503, not 401 — a
	// 401 would send the user to a login flow that can't work either
	token := mintToken(t, time.Hour)
	if resp := get(t, sec.URL+"/f13-protected", token); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 fail-closed, got %d", resp.StatusCode)
	}
}
