//go:build smoke

package smoke

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// An embedder's own program (examples/gohost: host.Run plus compiled-in Go
// aggregates and projections) serves them with no --tutorial; enforces the
// command permissions its embedded document declares, also after a hot
// reload (which swaps only the JS deciders' table); write-guards its
// projections' collections; and projects into them.
func TestGoHostServesItsCompiledInDomain(t *testing.T) {
	h := startBackendPkg(t, "github.com/jamestryand/pocketcqrs/examples/gohost", nil)

	var users struct {
		Fields []map[string]any `json:"fields"`
	}
	h.apiOK(http.MethodGet, "/api/collections/users", nil, &users)
	fields := append(users.Fields, map[string]any{"name": "role", "type": "text"})
	h.apiOK(http.MethodPatch, "/api/collections/users", jsonBody(map[string]any{"fields": fields}), nil)
	user := func(email, role string) string {
		h.apiOK(http.MethodPost, "/api/collections/users/records", jsonBody(map[string]any{
			"email": email, "password": "host-pass-1234", "passwordConfirm": "host-pass-1234", "role": role,
		}), nil)
		var out struct {
			Token string `json:"token"`
		}
		resp := h.do(http.MethodPost, h.BackendURL+"/api/collections/users/auth-with-password",
			jsonBody(map[string]string{"identity": email, "password": "host-pass-1234"}), nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("user sign-in failed: %s", resp.Status)
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Token
	}
	operator := user("operator@example.com", "operator")
	nobody := user("nobody@example.com", "")

	send := func(token, id, command string, body map[string]any) (int, string) {
		resp := h.do(http.MethodPost, h.BackendURL+"/api/cqrs/task/"+id+"/"+command, jsonBody(body),
			map[string]string{"Authorization": token})
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	title := map[string]any{"title": "write the docs"}

	check := func(when string) {
		t.Helper()
		if status, body := send(nobody, "a-"+when, "CreateTask", title); status != http.StatusForbidden {
			t.Fatalf("%s: no role: want 403, got %d %s", when, status, body)
		}
		if status, body := send(operator, "b-"+when, "CreateTask", title); status != http.StatusOK {
			t.Fatalf("%s: operator: want 200, got %d %s", when, status, body)
		}
		if status, body := send(h.Token, "c-"+when, "CreateTask", title); status != http.StatusOK {
			t.Fatalf("%s: superuser: want 200, got %d %s", when, status, body)
		}
		// CompleteTask is declared by no document: open to anyone signed in
		if status, body := send(nobody, "b-"+when, "CompleteTask", map[string]any{}); status != http.StatusOK {
			t.Fatalf("%s: undeclared command: want 200, got %d %s", when, status, body)
		}
	}
	check("boot")

	// a hot reload rebuilds the JS deciders' table; the compiled-in domain's
	// permissions are not in it and must survive
	h.setMode("maintenance")
	h.reload()
	h.setMode("running")
	check("reload")

	// the projection ran: the operator's task is in the read model
	deadline := time.Now().Add(10 * time.Second)
	for {
		var list struct {
			Items []map[string]any `json:"items"`
		}
		h.apiOK(http.MethodGet, "/api/collections/tasks/records?filter=title%3D%27write%20the%20docs%27", nil, &list)
		if len(list.Items) >= 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the compiled-in projection did not fill tasks: %v", list.Items)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// its collection is write-guarded, superusers included
	resp := h.do(http.MethodPost, h.BackendURL+"/api/collections/tasks/records",
		jsonBody(map[string]any{"title": "by hand"}), map[string]string{"Authorization": h.Token})
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(raw), "command") {
		t.Fatalf("a direct write to a compiled-in projection's collection: want 403 naming the command API, got %d %s",
			resp.StatusCode, raw)
	}
}
