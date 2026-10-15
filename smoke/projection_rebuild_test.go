//go:build smoke

package smoke

import (
	"net/http"
	"sort"
	"strings"
	"testing"
)

// TestProjectionRebuildIsCarriedOutByTheServer proves the rebuild wiring end
// to end: `projection rebuild` (a separate CLI process) and the admin route
// only RECORD a request in the node's data.db, and the running server's own
// engine wipes the projection and replays it. A rebuilt row is a new
// record (new id) holding the same values, which is how this test sees that
// the server, and nothing else, rebuilt it.
func TestProjectionRebuildIsCarriedOutByTheServer(t *testing.T) {
	const noteJS = `//@trigger decider note
//@handles NoteCreated
//@commands CreateNote

function initialState() { return { exists: false }; }
function decide(command, state) {
  if (command.name !== "CreateNote") throw new Error("unknown command: " + command.name);
  if (state.exists) throw new Error("note already exists");
  return [{ type: "NoteCreated", data: { title: command.payload.title } }];
}
function evolve(state, event) {
  if (event.type === "NoteCreated") { state.exists = true; }
  return state;
}
`
	const notesJS = `//@trigger projection notes on NoteCreated
//@schema notes noteId:text title:text
//@key noteId
function project(event) {
  return { upsert: { key: event.aggregateId, fields: { noteId: event.aggregateId, title: event.data.title } } };
}
`
	h := startBackendFlags(t, map[string]string{"note.js": noteJS, "notes.js": notesJS})
	h.command("note", "n1", "CreateNote", map[string]string{"title": "first"})
	h.command("note", "n2", "CreateNote", map[string]string{"title": "second"})

	type row struct {
		ID     string `json:"id"`
		NoteID string `json:"noteId"`
		Title  string `json:"title"`
	}
	rows := func() []row {
		var recs struct {
			Items []row `json:"items"`
		}
		status, _ := h.api(http.MethodGet, "/api/collections/notes/records", nil, &recs)
		if status != http.StatusOK {
			return nil
		}
		sort.Slice(recs.Items, func(i, j int) bool { return recs.Items[i].NoteID < recs.Items[j].NoteID })
		return recs.Items
	}
	var before []row
	eventually(t, "the projection to hold both notes", func() bool {
		before = rows()
		return len(before) == 2
	})
	rebuiltSince := func(prev []row) func() bool {
		return func() bool {
			now := rows()
			if len(now) != 2 {
				return false // wiped and still replaying: partial, as accepted
			}
			for i := range now {
				if now[i].NoteID != prev[i].NoteID || now[i].Title != prev[i].Title || now[i].ID == prev[i].ID {
					return false
				}
			}
			return true
		}
	}

	// the CLI records the request; the running server carries it out
	out := runCLI(t, h.Bin, h.DataDir, h.FunctionsDir, "projection", "rebuild", "notes")
	if !strings.Contains(out, `Rebuild of projection "notes" requested`) {
		t.Fatalf("unexpected CLI output:\n%s", out)
	}
	eventually(t, "the server to wipe and replay notes", rebuiltSince(before))

	// the admin route does the same
	again := rows()
	if status, raw := h.api(http.MethodPost, "/api/cqrs/admin/projections/notes/rebuild", nil, nil); status != http.StatusAccepted {
		t.Fatalf("admin rebuild: %d %s", status, truncate(raw, 300))
	}
	eventually(t, "the server to rebuild notes again", rebuiltSince(again))

	if status, _ := h.api(http.MethodPost, "/api/cqrs/admin/projections/nope/rebuild", nil, nil); status != http.StatusNotFound {
		t.Fatalf("an unknown projection: want 404, got %d", status)
	}
}
