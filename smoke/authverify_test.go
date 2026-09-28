//go:build smoke

package smoke

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// TestSecondaryVerifiesAuthAgainstMaster is the end-to-end proof for F-13's
// fix, from BOTH previously-broken directions in one flow:
//
//   - local-read direction: logging in via the secondary (forwarded, F-12)
//     yields a master-minted token, which used to make every authenticated
//     LOCAL read on the secondary fail — the exact regression that broke
//     TestSecondaryForwardsCommandsToMaster when --cqrsForwardAuth first
//     shipped. With --cqrsVerifyAuth the secondary verifies it against the
//     master instead.
//   - write-forwarding direction (the 2026-08-18 mirror image, found by
//     project/rotaboard): a token obtained by logging into the SECONDARY,
//     used for a command forwarded to master, used to get 401 there. Now
//     login forwards (implied --cqrsForwardAuth), so the token IS
//     master-minted and the forwarded write lands.
func TestSecondaryVerifiesAuthAgainstMaster(t *testing.T) {
	master := startBackend(t, nil) // --tutorial
	// startSecondary authenticates VIA the secondary at the end, so merely
	// getting a harness back already proves forwarded login works
	secondary := startSecondary(t, master, "--cqrsMasterAddr", master.BackendURL, "--cqrsVerifyAuth")

	// the exact read F-13 broke: superuser-gated, served locally from the
	// replicated events.db, gate now remote-verifying (fresh, shape C)
	var feed struct {
		Events []struct{ AggregateID string } `json:"events"`
	}
	secondary.apiOK(http.MethodGet, "/api/cqrs/events?aggregate=task", nil, &feed)

	// a route gated with PocketBase's own plain RequireSuperuserAuth — the
	// global cached middleware (shape C') is what populates re.Auth here;
	// twice, so the second hit rides the cached verdict
	secondary.apiOK(http.MethodGet, "/api/cqrs/catalog", nil, nil)
	secondary.apiOK(http.MethodGet, "/api/cqrs/catalog", nil, nil)

	// mirror direction: the same secondary-obtained token drives a
	// forwarded command, which master must accept
	secondary.command("task", "vt1", "CreateTask", map[string]string{"title": "verified write"})

	var masterStreams struct {
		Streams []struct{ AggregateID string } `json:"streams"`
	}
	master.apiOK(http.MethodGet, "/api/cqrs/streams?aggregate=task", nil, &masterStreams)
	found := false
	for _, s := range masterStreams.Streams {
		if s.AggregateID == "vt1" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected vt1 on the master: a secondary-login token must work for forwarded writes now")
	}

	// and the write replicates back into a read the SAME token can make
	// locally — the full loop no flag combination could close before
	eventually(t, "the secondary to see vt1 via replication, read with its own login's token", func() bool {
		status, _ := secondary.api(http.MethodGet, "/api/cqrs/events?aggregate=task", nil, &feed)
		if status != http.StatusOK {
			return false
		}
		for _, e := range feed.Events {
			if e.AggregateID == "vt1" {
				return true
			}
		}
		return false
	})
}

// TestSecondaryRevocationBitesOpsImmediately: rotating a record's TokenKey
// on the master — PocketBase's own per-user logout/revocation mechanism —
// must lock that token out of a mutating, superuser-only ops route
// promptly, because that gate re-verifies fresh on every request (shape C,
// no cache). This no longer holds for the five READ-ONLY ops routes since
// capability-verify-shape-decision.md (2026-09-28) moved them to shape C′ —
// see TestSecondaryOpsTierRevocationIsBoundedByItsOwnTTL below for that
// tier's own, deliberately bounded (not immediate) revocation lag.
func TestSecondaryRevocationBitesOpsImmediately(t *testing.T) {
	master := startBackend(t, nil)
	secondary := startSecondary(t, master, "--cqrsMasterAddr", master.BackendURL, "--cqrsVerifyAuth")

	// a second superuser, so revoking it cannot disturb the harness's own
	// token; created on master, logged in VIA the secondary (forwarded)
	const email = "revoke-me@example.com"
	const password = "revoke-pass-1234"
	master.apiOK(http.MethodPost, "/api/collections/_superusers/records",
		jsonBody(map[string]string{"email": email, "password": password, "passwordConfirm": password}), nil)

	resp := secondary.do(http.MethodPost, secondary.BackendURL+"/api/collections/_superusers/auth-with-password",
		jsonBody(map[string]string{"identity": email, "password": password}), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("login via secondary failed: %d: %s", resp.StatusCode, b)
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&login); err != nil {
		t.Fatal(err)
	}

	opsStatus := func() int {
		r := secondary.do(http.MethodGet, secondary.BackendURL+"/api/cqrs/admin/functions", nil,
			map[string]string{"Authorization": login.Token})
		defer r.Body.Close()
		return r.StatusCode
	}
	if got := opsStatus(); got != http.StatusOK {
		t.Fatalf("expected the fresh token to reach the ops surface, got %d", got)
	}

	rotateTokenKey(t, master.DataDir, email)

	eventually(t, "the revoked token to be rejected by the secondary's ops gate", func() bool {
		return opsStatus() == http.StatusUnauthorized
	})
}

// TestSecondaryOpsTierRevocationIsBoundedByItsOwnTTL is
// TestSecondaryRevocationBitesOpsImmediately's counterpart for the five
// read-only, capability-gated ops routes (capability-verify-shape-
// decision.md, 2026-09-28): a token revoked at the master keeps reaching
// them until the cached verdict's --cqrsOpsVerifyCacheTTL elapses — a
// deliberate, bounded revocation-lag tradeoff for outage tolerance, not a
// regression. A short TTL here keeps the eventual rejection well within
// eventually's 15s budget.
func TestSecondaryOpsTierRevocationIsBoundedByItsOwnTTL(t *testing.T) {
	master := startBackend(t, nil)
	secondary := startSecondary(t, master, "--cqrsMasterAddr", master.BackendURL,
		"--cqrsVerifyAuth", "--cqrsOpsVerifyCacheTTL", "2s")

	const email = "revoke-me-ops-tier@example.com"
	const password = "revoke-pass-1234"
	master.apiOK(http.MethodPost, "/api/collections/_superusers/records",
		jsonBody(map[string]string{"email": email, "password": password, "passwordConfirm": password}), nil)

	resp := secondary.do(http.MethodPost, secondary.BackendURL+"/api/collections/_superusers/auth-with-password",
		jsonBody(map[string]string{"identity": email, "password": password}), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("login via secondary failed: %d: %s", resp.StatusCode, b)
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&login); err != nil {
		t.Fatal(err)
	}

	opsTierStatus := func() int {
		r := secondary.do(http.MethodGet, secondary.BackendURL+"/api/cqrs/events", nil,
			map[string]string{"Authorization": login.Token})
		defer r.Body.Close()
		return r.StatusCode
	}
	if got := opsTierStatus(); got != http.StatusOK {
		t.Fatalf("expected the fresh token to reach the read-only ops tier, got %d", got)
	}

	rotateTokenKey(t, master.DataDir, email)

	// immediately after rotation, still within the ops TTL: the cached
	// verdict has not yet been re-checked, so this MUST still pass -- the
	// whole point of not calling the master on every read-only request
	if got := opsTierStatus(); got != http.StatusOK {
		t.Fatalf("expected the cached verdict to still pass immediately after rotation, got %d", got)
	}

	eventually(t, "the revoked token to be rejected once the ops tier's cached verdict expires", func() bool {
		return opsTierStatus() == http.StatusUnauthorized
	})
}

// rotateTokenKey performs PocketBase's revocation primitive directly against
// a node's live data.db (WAL allows the concurrent writer; same direct-SQL
// precedent as localSuperuserCount).
func rotateTokenKey(t *testing.T, dataDir, email string) {
	t.Helper()
	dsn := "file:" + filepath.Join(dataDir, "data.db") + "?_pragma=busy_timeout(10000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	res, err := db.Exec(`UPDATE _superusers SET tokenKey = lower(hex(randomblob(25))) WHERE email = ?`, email)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("expected to rotate exactly 1 tokenKey, rotated %d", n)
	}
}

// TestSecondaryVerifyCacheRidesOutMasterOutageThenFailsClosed: within the
// verdict TTL, every read-only ops route keeps serving with the master GONE
// (C′'s outage tolerance, capability-verify-shape-decision.md); a mutating,
// superuser-only ops route (never cached) answers 503 at once regardless;
// and past the TTL, with no grace configured, the cached routes fail closed
// as 503 too — not 401, which would send users to a login flow that also
// cannot work.
//
// --cqrsOpsVerifyCacheTTL governs the five read-only routes' freshness, not
// --cqrsVerifyCacheTTL (the general end-user TTL) — see the decision doc's
// "what's left to build" caveat about this test.
func TestSecondaryVerifyCacheRidesOutMasterOutageThenFailsClosed(t *testing.T) {
	master := startBackend(t, nil)
	secondary := startSecondary(t, master, "--cqrsMasterAddr", master.BackendURL,
		"--cqrsVerifyAuth", "--cqrsOpsVerifyCacheTTL", "4s")

	// prime the cached verdict via ONE of the five routes, then take the
	// master away
	secondary.apiOK(http.MethodGet, "/api/cqrs/catalog", nil, nil)
	master.stop()

	if status, body := secondary.api(http.MethodGet, "/api/cqrs/catalog", nil, nil); status != http.StatusOK {
		t.Fatalf("expected the cached verdict to serve through the outage, got %d: %s", status, body)
	}
	// the other four read-only routes share the SAME cache row (keyed by
	// token hash, not by route) -- catalog's warm-up serves them too,
	// without ever calling any of them before the outage
	for _, path := range []string{"/api/cqrs/events", "/api/cqrs/streams", "/api/cqrs/deadletters", "/api/cqrs/admin/mode"} {
		if status, body := secondary.api(http.MethodGet, path, nil, nil); status != http.StatusOK {
			t.Fatalf("expected %s to ride the outage on catalog's shared cache row, got %d: %s", path, status, body)
		}
	}
	// a mutating, superuser-only ops route is never cached (unchanged Shape
	// C) -- 503 immediately, master down or not, no warm-up possible
	if status, body := secondary.api(http.MethodPost, "/api/cqrs/admin/mode", jsonBody(map[string]string{"mode": "maintenance"}), nil); status != http.StatusServiceUnavailable {
		t.Fatalf("expected the fresh, mutating ops gate to answer 503 with master down, got %d: %s", status, body)
	}
	eventually(t, "the cached routes to fail closed as 503 once the verdict expires", func() bool {
		status, _ := secondary.api(http.MethodGet, "/api/cqrs/catalog", nil, nil)
		return status == http.StatusServiceUnavailable
	})
}

// TestSecondaryVerifyGraceServesThroughOutage: --cqrsVerifyGrace is the
// operator's opt-in to keep serving EXPIRED verdicts while the master is
// unreachable — the availability half of C′'s stated tradeoff, shared by
// the ops tier's own --cqrsOpsVerifyCacheTTL.
func TestSecondaryVerifyGraceServesThroughOutage(t *testing.T) {
	master := startBackend(t, nil)
	secondary := startSecondary(t, master, "--cqrsMasterAddr", master.BackendURL,
		"--cqrsVerifyAuth", "--cqrsOpsVerifyCacheTTL", "1s", "--cqrsVerifyGrace", "10m")

	secondary.apiOK(http.MethodGet, "/api/cqrs/catalog", nil, nil)
	master.stop()

	// well past the 1s TTL: without grace this is the fail-closed case the
	// previous test pins; with it, the stale verdict serves
	time.Sleep(2 * time.Second)
	if status, body := secondary.api(http.MethodGet, "/api/cqrs/catalog", nil, nil); status != http.StatusOK {
		t.Fatalf("expected the stale verdict to serve within grace, got %d: %s", status, body)
	}
	// another of the five, same shared cache row
	if status, body := secondary.api(http.MethodGet, "/api/cqrs/streams", nil, nil); status != http.StatusOK {
		t.Fatalf("expected streams to serve stale within grace too, got %d: %s", status, body)
	}
}
