//go:build smoke

package smoke

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSchemaImportRefusalExitsNonZero and TestSchemaImportRefusalWritesNoPbData
// prove F-9 and F-10's fix against a real built binary and a real process
// exit code -- neither is observable from an in-process cmd.Execute() call
// (schema_cmd_test.go's unit tests), since both bugs were specifically about
// what happens OUTSIDE the command's own RunE: PocketBase's bootstrap
// running before it (F-10), and pb.Execute() discarding RootCmd.Execute()'s
// return value on the way back out (F-9). Run from a fresh, empty working
// directory each time so a stray pb_data/ has nowhere to hide.
func TestSchemaImportRefusalExitsNonZero(t *testing.T) {
	bin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(t.TempDir(), "pocketcqrs"))
	dir := t.TempDir()

	bogus := filepath.Join(dir, "bogus.json")
	if err := os.WriteFile(bogus, []byte(`{"not":"a schema"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "schema", "import", "bogus.json")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("refused import exited 0; output:\n%s", out)
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() == 0 {
		t.Fatalf("expected a non-zero exit code, got %v; output:\n%s", err, out)
	}
}

func TestSchemaImportRefusalWritesNoPbData(t *testing.T) {
	bin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(t.TempDir(), "pocketcqrs"))
	dir := t.TempDir()

	bogus := filepath.Join(dir, "bogus.json")
	if err := os.WriteFile(bogus, []byte(`{"not":"a schema"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "schema", "import", "bogus.json")
	cmd.Dir = dir
	_, _ = cmd.CombinedOutput() // expected to fail; exit code covered separately above

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "pb_data" {
			t.Fatalf("a refused import created %s, a bootstrap side effect it should never have (F-10)",
				filepath.Join(dir, "pb_data"))
		}
	}
}
