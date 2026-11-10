//go:build smoke

package smoke

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const permTicket = `//@trigger decider ticket
//@handles TicketOpened TicketNoted
//@commands OpenTicket NoteTicket
//@authorize OpenTicket {"RequiredRole":["operator"]}
function initialState() { return {}; }
function decide(command, state) {
  // reads the name loosely on purpose: the policy must hold whatever the spelling
  const name = String(command.name).toLowerCase();
  if (name === 'openticket') return [{ type: 'TicketOpened', data: {} }];
  if (name === 'noteticket') return [{ type: 'TicketNoted', data: {} }];
  throw new Error('unknown command: ' + command.name);
}
function evolve(state, event) { return state; }
`

// The stock server enforces the command permissions its deciders declare,
// with nothing wired: a declared command is refused to a caller without the
// role, allowed with it and to a superuser; an undeclared command is
// unchanged; and a reload that adds a permission takes effect.
func TestStockServerEnforcesCommandPermissions(t *testing.T) {
	h := startBackendFlags(t, map[string]string{"ticket.js": permTicket})

	// a role field on users, set only by a superuser (pocketcqrs refuses it
	// on self-registration and self-update)
	var users struct {
		Fields []map[string]any `json:"fields"`
	}
	h.apiOK(http.MethodGet, "/api/collections/users", nil, &users)
	fields := append(users.Fields, map[string]any{"name": "role", "type": "text"})
	h.apiOK(http.MethodPatch, "/api/collections/users", jsonBody(map[string]any{"fields": fields}), nil)
	user := func(email, role string) string {
		h.apiOK(http.MethodPost, "/api/collections/users/records", jsonBody(map[string]any{
			"email": email, "password": "perm-pass-1234", "passwordConfirm": "perm-pass-1234", "role": role,
		}), nil)
		var out struct {
			Token string `json:"token"`
		}
		resp := h.do(http.MethodPost, h.BackendURL+"/api/collections/users/auth-with-password",
			jsonBody(map[string]string{"identity": email, "password": "perm-pass-1234"}), nil)
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

	send := func(token, id, command string) (int, string) {
		resp := h.do(http.MethodPost, h.BackendURL+"/api/cqrs/ticket/"+id+"/"+command, jsonBody(map[string]any{}),
			map[string]string{"Authorization": token})
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	if status, body := send(nobody, "t1", "OpenTicket"); status != http.StatusForbidden ||
		!strings.Contains(body, "not authorized to issue OpenTicket on ticket") {
		t.Fatalf("no role: want 403, got %d %s", status, body)
	}
	// spelling the aggregate or the command differently doesn't get round it
	for _, path := range []string{"/api/cqrs/Ticket/t1b/OpenTicket", "/api/cqrs/ticket/t1c/openticket", "/api/cqrs/ticket/t1d/OPENTICKET"} {
		resp := h.do(http.MethodPost, h.BackendURL+path, jsonBody(map[string]any{}), map[string]string{"Authorization": nobody})
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("%s by a caller without the role was accepted: %s", path, raw)
		}
	}
	if status, body := send(operator, "t2", "OpenTicket"); status != http.StatusOK {
		t.Fatalf("operator: want 200, got %d %s", status, body)
	}
	if status, body := send(h.Token, "t3", "OpenTicket"); status != http.StatusOK {
		t.Fatalf("superuser: want 200, got %d %s", status, body)
	}
	if status, body := send(nobody, "t4", "NoteTicket"); status != http.StatusOK {
		t.Fatalf("undeclared command: want 200 for anyone signed in, got %d %s", status, body)
	}
	if status, _ := send("", "t5", "NoteTicket"); status != http.StatusUnauthorized {
		t.Fatalf("anonymous: want 401 as before, got %d", status)
	}

	// a reload that declares NoteTicket's permission too takes effect
	withNote := strings.Replace(permTicket, "//@authorize OpenTicket",
		"//@authorize NoteTicket {\"RequiredRole\":[\"operator\"]}\n//@authorize OpenTicket", 1)
	if err := os.WriteFile(filepath.Join(h.FunctionsDir, "ticket.js"), []byte(withNote), 0o644); err != nil {
		t.Fatal(err)
	}
	h.setMode("maintenance")
	report := h.reload()
	h.setMode("running")
	if got, _ := report["commandPolicies"].(float64); got != 2 {
		t.Fatalf("reload should report 2 command policies, got %v (%v)", report["commandPolicies"], report)
	}
	if status, body := send(nobody, "t6", "NoteTicket"); status != http.StatusForbidden {
		t.Fatalf("after reload, NoteTicket needs the role: want 403, got %d %s", status, body)
	}
	if status, body := send(operator, "t7", "NoteTicket"); status != http.StatusOK {
		t.Fatalf("after reload, operator: want 200, got %d %s", status, body)
	}
}
