package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/packs"
)

// TestRequirePackCodeImportedRefusesBeforeReadingEventsFile is the unit
// half of events-db-slice-merge-scope.md §4's precondition: `events import`
// must refuse a pack whose code has not been imported yet, before ever
// looking at events.ndjson. The full end-to-end (a same-pack reactor
// starting from checkpoint 0 against imported history) is smoke-test
// territory; this proves the cheap file-existence gate itself.
func TestRequirePackCodeImportedRefusesOnMissingFunction(t *testing.T) {
	functionsDir := t.TempDir()
	manifest := &packs.Manifest{Name: "notes-domain", Functions: []string{"note.js", "notes.js"}}

	// neither file present yet
	if err := requirePackCodeImported(manifest, functionsDir); err == nil {
		t.Fatal("expected a refusal when no function files are present")
	}

	// one of the two present: still refused, and the error names the pack
	if err := os.WriteFile(filepath.Join(functionsDir, "note.js"), []byte("//"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := requirePackCodeImported(manifest, functionsDir)
	if err == nil {
		t.Fatal("expected a refusal when only one of two function files is present")
	}
	if !strings.Contains(err.Error(), "notes-domain") || !strings.Contains(err.Error(), "notes.js") {
		t.Fatalf("expected the error to name the pack and the missing file, got: %v", err)
	}
}

func TestRequirePackCodeImportedSucceedsWhenAllPresent(t *testing.T) {
	functionsDir := t.TempDir()
	manifest := &packs.Manifest{Name: "notes-domain", Functions: []string{"note.js", "notes.js"}}
	for _, name := range manifest.Functions {
		if err := os.WriteFile(filepath.Join(functionsDir, name), []byte("//"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := requirePackCodeImported(manifest, functionsDir); err != nil {
		t.Fatalf("expected success once every function file is present: %v", err)
	}
}
