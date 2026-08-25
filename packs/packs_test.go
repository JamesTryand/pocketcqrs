package packs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func writeFn(t *testing.T, dir, name, src string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

const noteDeciderSrc = `//@trigger decider note
//@handles NoteCreated
function initialState() { return { exists: false }; }
function decide(command, state) { return [{ type: "NoteCreated", data: {} }]; }
function evolve(state, event) { return state; }
`

const noteProjSrc = `//@trigger projection notes on NoteCreated
//@schema notes noteId:text
//@key noteId
function project(event) { return { upsert: { key: event.aggregateId, fields: {} } }; }
`

func TestExportImportRoundTrip(t *testing.T) {
	srcApp, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer srcApp.Cleanup()

	// a plain collection to pack
	if err := srcApp.Save(core.NewBaseCollection("labels")); err != nil {
		t.Fatal(err)
	}

	srcFns := t.TempDir()
	writeFn(t, srcFns, "note.js", noteDeciderSrc)
	writeFn(t, srcFns, "notes.js", noteProjSrc)

	packDir := filepath.Join(t.TempDir(), "notes-pack")
	manifest, err := Export(srcApp, srcFns, packDir, ExportOptions{
		Name:               "notes-domain",
		Version:            "1.2.0",
		Description:        "notes vertical",
		Functions:          []string{"note.js", "notes.js"},
		Collections:        []string{"labels"},
		GuardedCollections: []string{"notes"},
		Aggregates:         []string{"note"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Name != "notes-domain" || manifest.Version != "1.2.0" || len(manifest.Functions) != 2 {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	if len(manifest.Aggregates) != 1 || manifest.Aggregates[0] != "note" {
		t.Fatalf("expected Aggregates to round-trip into the manifest, got %+v", manifest.Aggregates)
	}
	for _, name := range []string{"manifest.json", "collections.json", "pb_functions/note.js", "pb_functions/notes.js"} {
		if _, err := os.Stat(filepath.Join(packDir, name)); err != nil {
			t.Fatalf("pack file missing: %s", name)
		}
	}

	// import into a fresh app + functions dir
	dstApp, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer dstApp.Cleanup()
	dstFns := t.TempDir()

	result, err := Import(dstApp, dstFns, packDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.FunctionsCopied) != 2 || result.CollectionsImported != 1 {
		t.Fatalf("unexpected import result: %+v", result)
	}
	if len(result.Manifest.Aggregates) != 1 || result.Manifest.Aggregates[0] != "note" {
		t.Fatalf("expected Aggregates to survive Import, got %+v", result.Manifest.Aggregates)
	}
	if _, err := dstApp.FindCollectionByNameOrId("labels"); err != nil {
		t.Fatalf("labels collection not imported: %v", err)
	}

	// second import without force: functions skipped
	result2, err := Import(dstApp, dstFns, packDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result2.FunctionsCopied) != 0 || len(result2.FunctionsSkipped) != 2 {
		t.Fatalf("expected skips, got %+v", result2)
	}
}

func TestExportRejections(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	fns := t.TempDir()
	writeFn(t, fns, "note.js", noteDeciderSrc)

	// no name
	if _, err := Export(app, fns, filepath.Join(t.TempDir(), "p"), ExportOptions{}); err == nil {
		t.Fatal("expected name rejection")
	}

	// guarded (projection-owned) collection refused
	if err := app.Save(core.NewBaseCollection("notes")); err != nil {
		t.Fatal(err)
	}
	_, err = Export(app, fns, filepath.Join(t.TempDir(), "p"), ExportOptions{
		Name: "x", Collections: []string{"notes"}, GuardedCollections: []string{"notes"},
	})
	if err == nil {
		t.Fatal("expected guarded collection rejection")
	}

	// a pack with a broken file refuses to export
	writeFn(t, fns, "broken.js", "//@trigger http\nfunction handle( {\n")
	_, err = Export(app, fns, filepath.Join(t.TempDir(), "p"), ExportOptions{Name: "x"})
	if err == nil {
		t.Fatal("expected load-validation rejection")
	}
}

// TestImportOldManifestWithoutAggregates proves a pack directory written
// before the Aggregates field existed still imports unchanged (Manifest.
// Aggregates is nil, not an error) — the "optional and ignored, no behavior
// change" claim for pre-existing packs, checked rather than assumed.
func TestImportOldManifestWithoutAggregates(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	dir := t.TempDir()
	writeFn(t, dir, "manifest.json", `{"name":"x","functions":["a.js"]}`)
	if err := os.MkdirAll(filepath.Join(dir, "pb_functions"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFn(t, filepath.Join(dir, "pb_functions"), "a.js", noteDeciderSrc)

	result, err := Import(app, t.TempDir(), dir, false)
	if err != nil {
		t.Fatalf("old-style manifest without aggregates should still import: %v", err)
	}
	if result.Manifest.Aggregates != nil {
		t.Fatalf("expected nil Aggregates for a manifest that never declared them, got %+v", result.Manifest.Aggregates)
	}
}

func TestReadManifest(t *testing.T) {
	dir := t.TempDir()
	writeFn(t, dir, "manifest.json", `{"name":"x","functions":["a.js"],"aggregates":["task"]}`)

	m, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "x" || len(m.Aggregates) != 1 || m.Aggregates[0] != "task" {
		t.Fatalf("unexpected manifest: %+v", m)
	}

	if _, err := ReadManifest(t.TempDir()); err == nil {
		t.Fatal("expected an error for a missing manifest.json")
	}

	badDir := t.TempDir()
	writeFn(t, badDir, "manifest.json", `{"functions":["a.js"]}`)
	if _, err := ReadManifest(badDir); err == nil {
		t.Fatal("expected an error for a manifest without a name")
	}
}

func TestImportRejectsBrokenManifest(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	// missing manifest
	if _, err := Import(app, t.TempDir(), t.TempDir(), false); err == nil {
		t.Fatal("expected missing manifest error")
	}

	// manifest without functions
	dir := t.TempDir()
	writeFn(t, dir, "manifest.json", `{"name":"x"}`)
	if _, err := Import(app, t.TempDir(), dir, false); err == nil {
		t.Fatal("expected empty-functions rejection")
	}
}
