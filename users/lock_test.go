package users

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

// usersAPI serves app's real PocketBase record API.
func usersAPI(t *testing.T, app core.App) *httptest.Server {
	t.Helper()
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	mux, err := router.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

// A read model's access rules read the caller's role and subject id from
// their auth record. The users collection's own rules let anyone register
// and let a user update their own record, so with a role field on it a user
// could register as, or promote themselves to, any role and read whatever
// that role reads. LockFields makes the self-service rules refuse those
// fields; everything else stays self-service, and a superuser still sets
// them.
func TestLockedFieldsCannotBeSelfAssigned(t *testing.T) {
	app := freshApp(t)
	if err := ensureCollection(app); err != nil {
		t.Fatal(err)
	}
	col, err := app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		t.Fatal(err)
	}
	col.Fields.Add(&core.TextField{Name: "role"}, &core.TextField{Name: "staffId"}, &core.TextField{Name: "nickname"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	if err := LockFields(app, "role", "staffId"); err != nil {
		t.Fatal(err)
	}
	// idempotent: a second boot adds nothing
	if err := LockFields(app, "role", "staffId"); err != nil {
		t.Fatal(err)
	}
	srv := usersAPI(t, app)
	const path = "/api/collections/users/records"

	creds := map[string]any{"email": "a@example.com", "password": "secret-password", "passwordConfirm": "secret-password"}
	withRole := map[string]any{"role": "manager"}
	for k, v := range creds {
		withRole[k] = v
	}
	if status, _ := call(t, srv, http.MethodPost, path, "", withRole); status < 400 {
		t.Fatalf("registering with a role must be refused, got %d", status)
	}
	status, rec := call(t, srv, http.MethodPost, path, "", creds)
	if status != http.StatusOK {
		t.Fatalf("plain registration still works: %d %v", status, rec)
	}
	id, _ := rec["id"].(string)

	status, auth := call(t, srv, http.MethodPost, "/api/collections/users/auth-with-password", "",
		map[string]any{"identity": "a@example.com", "password": "secret-password"})
	if status != http.StatusOK {
		t.Fatalf("sign-in: %d %v", status, auth)
	}
	token, _ := auth["token"].(string)

	for _, field := range []string{"role", "staffId"} {
		if status, _ := call(t, srv, http.MethodPatch, path+"/"+id, token, map[string]any{field: "x"}); status < 400 {
			t.Errorf("a user must not set their own %s, got %d", field, status)
		}
	}
	if status, _ := call(t, srv, http.MethodPatch, path+"/"+id, token, map[string]any{"nickname": "al"}); status != http.StatusOK {
		t.Errorf("other fields stay self-service, got %d", status)
	}

	// a superuser (or a command, server-side) still sets them
	stored, err := app.FindRecordById(CollectionName, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.GetString("role") != "" {
		t.Fatalf("the role was set by the user: %q", stored.GetString("role"))
	}
	superuser, err := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	suToken, err := superuser.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := call(t, srv, http.MethodPatch, path+"/"+id, suToken, map[string]any{"role": "manager"}); status != http.StatusOK {
		t.Errorf("a superuser sets the role, got %d", status)
	}
}

// A rule the app already tightened is kept, with the lock added; a
// superuser-only rule (nil) stays as it is.
func TestLockFieldsKeepsTheAppsOwnRule(t *testing.T) {
	app := freshApp(t)
	if err := ensureCollection(app); err != nil {
		t.Fatal(err)
	}
	col, _ := app.FindCollectionByNameOrId(CollectionName)
	invite := `@request.body.inviteCode = "x"`
	col.CreateRule = &invite
	col.UpdateRule = nil
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	if err := LockFields(app, "role"); err != nil {
		t.Fatal(err)
	}
	col, _ = app.FindCollectionByNameOrId(CollectionName)
	want := `(@request.body.inviteCode = "x") && @request.body.role:isset = false`
	if col.CreateRule == nil || *col.CreateRule != want {
		t.Errorf("CreateRule = %v, want %q", col.CreateRule, want)
	}
	if col.UpdateRule != nil {
		t.Errorf("a superuser-only UpdateRule stays nil, got %q", *col.UpdateRule)
	}
}
