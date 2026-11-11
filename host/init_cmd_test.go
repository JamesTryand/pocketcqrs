package host

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoImportPathFindsTheModuleAbove(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/app\n\ngo 1.25.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{".": "example.com/app", "domain": "example.com/app/domain", "a/b": "example.com/app/a/b"} {
		got, ok, err := goImportPath(filepath.Join(root, rel))
		if err != nil || !ok || got != want {
			t.Errorf("%s: got %q %v %v, want %q", rel, got, ok, err, want)
		}
	}
}

func runInit(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newInitCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// init go writes a program of one's own that depends on pocketcqrs as a module
// (required and a tool), never its source, with the generated wiring.
func TestInitGoWritesAProgramOfItsOwn(t *testing.T) {
	repo, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "app")
	out, err := runInit(t, "go", "example.com/app", "--model", filepath.Join(repo, "examples", "gohost", "task.eventmodel.json"),
		"--dir", dir, "--version", "v1.2.3")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"module example.com/app", "tool github.com/jamestryand/pocketcqrs", "require github.com/jamestryand/pocketcqrs v1.2.3"} {
		if !strings.Contains(string(mod), want) {
			t.Errorf("go.mod lacks %q:\n%s", want, mod)
		}
	}
	if strings.Contains(string(mod), "replace") {
		t.Errorf("a named version must not build against a checkout:\n%s", mod)
	}
	for _, f := range []string{"main.go", "generate.go", "Dockerfile", ".gitignore", "README.md",
		"model/task.eventmodel.json", "domain/register.go", "domain/model.eventmodel.json", "domain/task/task.go"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	gen, _ := os.ReadFile(filepath.Join(dir, "generate.go"))
	if !strings.Contains(string(gen), "//go:generate go tool pocketcqrs schema import model/task.eventmodel.json --lang go --out domain --force") {
		t.Errorf("generate.go:\n%s", gen)
	}
	reg, _ := os.ReadFile(filepath.Join(dir, "domain", "register.go"))
	if !strings.Contains(string(reg), `task "example.com/app/domain/task"`) {
		t.Errorf("register.go must import its packages by the module's path:\n%s", reg)
	}
}

func TestInitGoRefusesANonEmptyDirectoryAndABadModule(t *testing.T) {
	repo, _ := filepath.Abs("..")
	model := filepath.Join(repo, "examples", "gohost", "task.eventmodel.json")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runInit(t, "go", "example.com/app", "--model", model, "--dir", dir, "--version", "v1.2.3"); err == nil ||
		!strings.Contains(err.Error(), "not empty") {
		t.Fatalf("want a refusal for a non-empty directory, got %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "keep.txt")); string(raw) != "mine" {
		t.Fatal("an existing file was touched")
	}
	if _, err := runInit(t, "go", "not a module", "--model", model, "--dir", filepath.Join(dir, "x"), "--version", "v1.2.3"); err == nil {
		t.Fatal("want a refusal for a bad module path")
	}
}
