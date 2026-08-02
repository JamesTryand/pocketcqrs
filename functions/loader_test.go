package functions

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeFn(t *testing.T, dir, name, src string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	writeFn(t, dir, "audit.js", "//@trigger event TaskCreated TaskCompleted\nconsole.log(event.type);\n")
	writeFn(t, dir, "hello.js", "//@trigger http\nfunction handle(request) { return {message: 'hi'}; }\n")
	writeFn(t, dir, "ignored.js", "console.log('no directive');\n")
	writeFn(t, dir, "notes.txt", "not a function\n")

	rt := NewGojaRuntime(nil)
	httpReg, err := LoadDir(rt, dir)
	if err != nil {
		t.Fatal(err)
	}

	if got := len(rt.Consumers()); got != 1 {
		t.Fatalf("expected 1 event consumer, got %d", got)
	}
	if names := httpReg.Names(); !slices.Equal(names, []string{"hello"}) {
		t.Fatalf("unexpected http functions: %v", names)
	}
}

func TestLoadDirMissing(t *testing.T) {
	rt := NewGojaRuntime(nil)
	reg, err := LoadDir(rt, filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Names()) != 0 {
		t.Fatal("expected empty registry")
	}
}

func TestParseTriggers(t *testing.T) {
	types, isHTTP := parseTriggers("//@trigger event A B\n//@trigger http\nconsole.log(1)\n")
	if !slices.Equal(types, []string{"A", "B"}) || !isHTTP {
		t.Fatalf("got types=%v http=%v", types, isHTTP)
	}

	// directives must lead the file
	types, isHTTP = parseTriggers("console.log(1)\n//@trigger event A\n")
	if len(types) != 0 || isHTTP {
		t.Fatalf("late directives should be ignored, got types=%v http=%v", types, isHTTP)
	}
}
