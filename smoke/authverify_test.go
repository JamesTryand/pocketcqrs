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
// must lock that token out of the secondary's ops surface promptly, because
// the ops gate re-verifies fresh on every request (shape C, no cache).
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
		r := secondary.do(http.MethodGet, secondary.BackendURL+"/api/cqrs/events", nil,
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
// verdict TTL a cached route keeps serving with the master GONE (C′'s
// outage tolerance); the fresh ops gate answers 503 at once; and past the
// TTL, with no grace configured, the cached route fails closed as 503 too —
// not 401, which would send users to a login flow that also cannot work.
func TestSecondaryVerifyCacheRidesOutMasterOutageThenFailsClosed(t *testing.T) {
	master := startBackend(t, nil)
	secondary := startSecondary(t, master, "--cqrsMasterAddr", master.BackendURL,
		"--cqrsVerifyAuth", "--cqrsVerifyCacheTTL", "4s")

	// prime the cached verdict, then take the master away
	secondary.apiOK(http.MethodGet, "/api/cqrs/catalog", nil, nil)
	master.stop()

	if status, body := secondary.api(http.MethodGet, "/api/cqrs/catalog", nil, nil); status != http.StatusOK {
		t.Fatalf("expected the cached verdict to serve through the outage, got %d: %s", status, body)
	}
	if status, body := secondary.api(http.MethodGet, "/api/cqrs/events", nil, nil); status != http.StatusServiceUnavailable {
		t.Fatalf("expected the fresh ops gate to answer 503 with master down, got %d: %s", status, body)
	}
	eventually(t, "the cached route to fail closed as 503 once the verdict expires", func() bool {
		status, _ := secondary.api(http.MethodGet, "/api/cqrs/catalog", nil, nil)
		return status == http.StatusServiceUnavailable
	})
}

// TestSecondaryVerifyGraceServesThroughOutage: --cqrsVerifyGrace is the
// operator's opt-in to keep serving EXPIRED verdicts while the master is
// unreachable — the availability half of C′'s stated tradeoff.
func TestSecondaryVerifyGraceServesThroughOutage(t *testing.T) {
	master := startBackend(t, nil)
	secondary := startSecondary(t, master, "--cqrsMasterAddr", master.BackendURL,
		"--cqrsVerifyAuth", "--cqrsVerifyCacheTTL", "1s", "--cqrsVerifyGrace", "10m")

	secondary.apiOK(http.MethodGet, "/api/cqrs/catalog", nil, nil)
	master.stop()

	// well past the 1s TTL: without grace this is the fail-closed case the
	// previous test pins; with it, the stale verdict serves
	time.Sleep(2 * time.Second)
	if status, body := secondary.api(http.MethodGet, "/api/cqrs/catalog", nil, nil); status != http.StatusOK {
		t.Fatalf("expected the stale verdict to serve within grace, got %d: %s", status, body)
	}
}
