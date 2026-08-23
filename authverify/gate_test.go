package authverify

import (
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// newRoleRecord creates a fresh non-superuser auth record on a throwaway
// "roles" collection with the given capabilities, and mints a real token
// for it. Defined here (not imported from the roles package) to keep
// authverify's tests free of a dependency on it — the whole point of
// RequireCapability is that it works for ANY auth collection carrying a
// capabilities field, not specifically the roles one.
func newRoleRecord(t *testing.T, app core.App, email string, capabilities []string) (*core.Record, string) {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("roles")
	if err != nil {
		col = core.NewAuthCollection("roles")
		col.Fields.Add(&core.JSONField{Name: "capabilities"})
		if err := app.Save(col); err != nil {
			t.Fatal(err)
		}
	}
	rec := core.NewRecord(col)
	rec.SetEmail(email)
	rec.SetPassword("1234567890")
	rec.Set("capabilities", capabilities)
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}
	token, err := rec.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	return rec, token
}

func bindCapabilityRoute(e *core.ServeEvent, v *Verifier, capability string) {
	e.Router.GET("/cap", func(re *core.RequestEvent) error {
		return re.JSON(http.StatusOK, map[string]string{"as": re.Auth.Id})
	}).Bind(RequireCapability(v, capability))
}

// TestRequireCapabilityLocal covers v == nil, the common single-node case.
func TestRequireCapabilityLocal(t *testing.T) {
	app := openTestApp(t)
	_, superToken := newSuperuser(t, app, "cap-super@example.com")
	_, grantedToken := newRoleRecord(t, app, "cap-granted@example.com", []string{"ops.events.read"})
	_, ungrantedToken := newRoleRecord(t, app, "cap-ungranted@example.com", []string{"ops.streams.read"})
	srv := serveNode(t, app, func(e *core.ServeEvent) { bindCapabilityRoute(e, nil, "ops.events.read") })

	// superuser parity: a genuine superuser passes ANY capability check,
	// exactly as RequireSuperuser would -- the property the catalog route
	// rebind depends on to be a no-op for existing superuser deployments
	if resp := get(t, srv.URL+"/cap", superToken); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected superuser to pass, got %d", resp.StatusCode)
	}
	if resp := get(t, srv.URL+"/cap", grantedToken); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the granted capability to pass, got %d", resp.StatusCode)
	}
	if resp := get(t, srv.URL+"/cap", ungrantedToken); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected the wrong capability to be forbidden, got %d", resp.StatusCode)
	}
	if resp := get(t, srv.URL+"/cap", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no token, got %d", resp.StatusCode)
	}
}

// TestRequireCapabilityRemote is the advisor-flagged case: a "roles"
// record's capabilities field must actually survive the master's verdict
// serialization and Materialize round trip, not just work locally where
// re.Auth is populated by ordinary PocketBase middleware. Proven against a
// REAL master node (RegisterEndpoint), not a stand-in, so a hidden-field or
// serialization regression would be caught here.
func TestRequireCapabilityRemote(t *testing.T) {
	mApp := openTestApp(t)
	_, superToken := newSuperuser(t, mApp, "cap-remote-super@example.com")
	_, grantedToken := newRoleRecord(t, mApp, "cap-remote-granted@example.com", []string{"ops.catalog.read"})
	_, ungrantedToken := newRoleRecord(t, mApp, "cap-remote-ungranted@example.com", []string{"ops.events.read"})
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
	v := New(base, cache, 5*time.Minute, 0)

	// secondary: same collections (roles, _superusers), NOT the same rows --
	// exactly the F-13 topology, now exercised for a non-superuser collection
	sApp := openTestApp(t)
	// only needed to create the local "roles" collection shell so
	// Materialize has somewhere to bind the verdict's record into; the row
	// itself is irrelevant and discarded
	newRoleRecord(t, sApp, "unused@example.com", nil)
	sec := serveNode(t, sApp, func(e *core.ServeEvent) { bindCapabilityRoute(e, v, "ops.catalog.read") })

	if resp := get(t, sec.URL+"/cap", superToken); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected a remotely-verified superuser to pass, got %d", resp.StatusCode)
	}
	if resp := get(t, sec.URL+"/cap", grantedToken); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the remotely-verified granted capability to pass -- if this fails, "+
			"the capabilities field did not survive the verdict's serialization, got %d", resp.StatusCode)
	}
	if resp := get(t, sec.URL+"/cap", ungrantedToken); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected the remotely-verified wrong capability to be forbidden, got %d", resp.StatusCode)
	}

	// revocation bites immediately here too, same as RequireSuperuser:
	// VerifyFresh re-serializes the record on every call, so an edit to
	// capabilities (not just a token revocation) takes effect at once
	grantedRec, err := mApp.FindAuthRecordByToken(grantedToken, core.TokenTypeAuth)
	if err != nil {
		t.Fatal(err)
	}
	grantedRec.Set("capabilities", []string{})
	if err := mApp.Save(grantedRec); err != nil {
		t.Fatal(err)
	}
	if resp := get(t, sec.URL+"/cap", grantedToken); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected a capability removed at the master to bite immediately, got %d", resp.StatusCode)
	}
}
