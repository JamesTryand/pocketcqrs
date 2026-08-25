//go:build smoke

package smoke

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEventsSliceMergeSkipsEffectTierReplaysProjections is the end-to-end
// proof of events-db-slice-merge-scope.md: export a pack's committed event
// history from a live SOURCE deployment, import it into a fresh TARGET
// deployment that already has the same pack's code active, and confirm
// that starting the target does NOT re-fire its reactor or effect function
// against the imported history, while its projection DOES materialize the
// imported data. Unit tests already prove the underlying mechanism
// (packs.ImportEvents' checkpoint fast-forward, TestImportEventsAdvances
// EffectTierNotProjections) against hand-constructed consumer names; this
// test is the thing those cannot be: real function files, loaded by a real
// bootstrapped app, producing real "reactor:"/"fn-reactor:"/"fn:" checkpoint
// keys through the actual loader/registry/engine wiring, moved between two
// real, separate data directories via the real CLI commands an operator
// would run. The worst defects in this project live in exactly that wiring.
func TestEventsSliceMergeSkipsEffectTierReplaysProjections(t *testing.T) {
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
	// counter is deliberately NOT idempotent (no guard against a second
	// Increment) and is NOT one of the exported/imported aggregates —
	// the target starts with zero counter history, so any counter stream
	// appearing there at all can only mean the reactor re-fired against
	// imported note history it should have skipped.
	const counterJS = `//@trigger decider counter
//@handles Incremented
//@commands Increment

function initialState() { return { count: 0 }; }
function decide(command, state) {
  if (command.name !== "Increment") throw new Error("unknown command: " + command.name);
  return [{ type: "Incremented", data: {} }];
}
function evolve(state, event) {
  if (event.type === "Incremented") { state.count = state.count + 1; }
  return state;
}
`
	const notereactorJS = `//@trigger reactor NoteCreated
//@dispatches counter/Increment

function reactTo(event) {
  return [{ aggregate: "counter", id: "note-counter", command: "Increment", payload: {} }];
}
`
	const noteauditJS = `//@trigger event NoteCreated
console.log("note created: " + event.aggregateId);
`
	functions := map[string]string{
		"note.js":        noteJS,
		"notes.js":       notesJS,
		"counter.js":     counterJS,
		"notereactor.js": notereactorJS,
		"noteaudit.js":   noteauditJS,
	}

	// ---- SOURCE: a live deployment that actually processes commands ----
	src := startBackendFlags(t, functions)
	src.command("note", "n1", "CreateNote", map[string]string{"title": "first"})
	src.command("note", "n2", "CreateNote", map[string]string{"title": "second"})

	// sanity: the reactor is real and fired live, twice, on the source
	eventually(t, "source reactor to fire twice", func() bool {
		var out struct {
			Streams []struct {
				Events int64 `json:"events"`
			} `json:"streams"`
		}
		status, _ := src.api(http.MethodGet, "/api/cqrs/streams?aggregate=counter", nil, &out)
		return status == http.StatusOK && len(out.Streams) == 1 && out.Streams[0].Events == 2
	})

	src.stop() // pack/events export run offline, against the now-idle data dir

	// ---- pack + event-data export (source's own data dir) ----
	packDir := filepath.Join(t.TempDir(), "notes-pack")
	runCLI(t, src.Bin, src.DataDir, src.FunctionsDir,
		"pack", "export", packDir,
		"--name", "notes-domain", "--aggregates", "note")
	runCLI(t, src.Bin, src.DataDir, src.FunctionsDir, "events", "export", packDir)

	if _, err := os.Stat(filepath.Join(packDir, "events.ndjson")); err != nil {
		t.Fatalf("events export did not write events.ndjson: %v", err)
	}

	// ---- TARGET: fresh deployment, pack code imported BEFORE event data ----
	targetDir := t.TempDir()
	targetData := filepath.Join(targetDir, "pb_data")
	targetFns := filepath.Join(targetDir, "pb_functions")
	if err := os.MkdirAll(targetFns, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := exec.Command(src.Bin, "superuser", "upsert", superuserEmail, superuserPassword, "--dir", targetData)
	if out, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seeding the target superuser failed: %v\n%s", err, out)
	}

	runCLI(t, src.Bin, targetData, targetFns, "pack", "import", packDir)

	// the precondition refuses BEFORE the pack's own code is in place —
	// proven directly against a functionsDir that does not have it yet.
	// Asserted on OUTPUT content, not the process exit code: this repo has
	// a pre-existing, unrelated gap (confirmed independently against
	// `pack import` with a bad packDir) where app.Start() discards a RunE
	// error's exit code for every bootstrap-requiring CLI command -- the
	// same F-9 class main.go already documents fixing for `schema import`
	// specifically, but nothing else. Out of scope here; see NEEDS.md.
	emptyFns := t.TempDir()
	out, _ := exec.Command(src.Bin, "events", "import", packDir, "--dir", targetData, "--functionsDir", emptyFns).CombinedOutput()
	if !strings.Contains(string(out), "import the pack's code and reload") {
		t.Fatalf("expected the precondition's pointed error, got:\n%s", out)
	}
	if strings.Contains(string(out), "Imported ") {
		t.Fatalf("expected NOTHING imported when the precondition refuses, got:\n%s", out)
	}

	// --dry-run against the REAL target: predicts the same shape a real
	// import will produce, without writing anything
	dryOut := runCLI(t, src.Bin, targetData, targetFns, "events", "import", packDir, "--dry-run")
	if !strings.Contains(dryOut, "dry run, nothing written") {
		t.Fatalf("expected the dry-run banner, got:\n%s", dryOut)
	}
	if !strings.Contains(dryOut, "fn-reactor:notereactor") || !strings.Contains(dryOut, "fn:noteaudit") {
		t.Fatalf("expected the dry run to name both real effect-tier consumers, got:\n%s", dryOut)
	}
	if strings.Contains(dryOut, "\n  notes ") || strings.Contains(dryOut, " notes\n") {
		t.Fatalf("expected the JS projection (checkpoint name \"notes\", no prefix) to be absent from the advanced list, got:\n%s", dryOut)
	}

	realOut := runCLI(t, src.Bin, targetData, targetFns, "events", "import", packDir)
	if strings.Contains(realOut, "dry run") {
		t.Fatalf("expected a real import to carry no dry-run banner, got:\n%s", realOut)
	}
	if !strings.Contains(realOut, "fn-reactor:notereactor") || !strings.Contains(realOut, "fn:noteaudit") {
		t.Fatalf("expected the real import to advance both real effect-tier consumers, got:\n%s", realOut)
	}
	if !strings.Contains(realOut, "Imported 2 event(s)") {
		t.Fatalf("expected 2 imported events, got:\n%s", realOut)
	}

	// ---- start the target for real and let it catch up ----
	addr := freeAddr(t)
	target := &harness{t: t, BackendURL: "http://" + addr, FunctionsDir: targetFns, DataDir: targetData, Bin: src.Bin, client: newClient(t)}
	target.stop = serve(t, src.Bin, targetDir, "target",
		"serve", "--http", addr, "--dir", targetData, "--functionsDir", targetFns)
	waitFor(t, target.BackendURL+"/api/health")
	target.Token = target.authenticate()

	// the projection DID replay: both imported notes show up
	eventually(t, "target projection to materialize both imported notes", func() bool {
		var recs struct {
			Items []struct {
				Title string `json:"title"`
			} `json:"items"`
		}
		status, _ := target.api(http.MethodGet, "/api/collections/notes/records", nil, &recs)
		return status == http.StatusOK && len(recs.Items) == 2
	})

	// the reactor did NOT re-fire: the counter aggregate — never part of
	// the exported/imported aggregate set — has zero streams on the target
	var counterStreams struct {
		Streams []any `json:"streams"`
	}
	if status, raw := target.api(http.MethodGet, "/api/cqrs/streams?aggregate=counter", nil, &counterStreams); status != http.StatusOK {
		t.Fatalf("streams query failed: %d: %s", status, truncate(raw, 300))
	}
	if len(counterStreams.Streams) != 0 {
		t.Fatalf("the reactor re-fired against imported history — checkpoint fast-forward did not hold: %+v", counterStreams.Streams)
	}
}

// runCLI runs a one-shot pocketcqrs subcommand (not `serve`) against dataDir
// and functionsDir, failing the test on a non-zero exit, and returns
// combined stdout+stderr for output-shape assertions.
func runCLI(t *testing.T, bin, dataDir, functionsDir string, args ...string) string {
	t.Helper()
	full := append(append([]string{}, args...), "--dir", dataDir, "--functionsDir", functionsDir)
	cmd := exec.Command(bin, full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", bin, strings.Join(full, " "), err, out)
	}
	return string(out)
}
