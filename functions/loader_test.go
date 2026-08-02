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
	loaded, err := LoadDir(rt, nil, dir)
	if err != nil {
		t.Fatal(err)
	}

	if got := len(rt.Consumers()); got != 1 {
		t.Fatalf("expected 1 event consumer, got %d", got)
	}
	if names := loaded.HTTP.Names(); !slices.Equal(names, []string{"hello"}) {
		t.Fatalf("unexpected http functions: %v", names)
	}
	if len(loaded.Projections) != 0 || len(loaded.Deciders) != 0 {
		t.Fatalf("unexpected specs: %+v", loaded)
	}
}

func TestLoadDirProjection(t *testing.T) {
	dir := t.TempDir()
	writeFn(t, dir, "rollup.js", `//@trigger projection orders_by_customer on OrderPlaced
//@schema orders_by_customer customerRef:text orderCount:number
//@key customerRef
function project(event) { return; }
`)

	rt := NewGojaRuntime(nil)
	loaded, err := LoadDir(rt, nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	projs := loaded.Projections
	if len(projs) != 1 {
		t.Fatalf("expected 1 projection, got %d", len(projs))
	}
	p := projs[0]
	if p.Name != "orders_by_customer" {
		t.Fatalf("unexpected name: %s", p.Name)
	}
	if p.Schema.Collection != "orders_by_customer" || p.Schema.Key != "customerRef" {
		t.Fatalf("unexpected schema: %+v", p.Schema)
	}
	if len(p.Schema.Fields) != 2 || p.Schema.Fields[1].Type != "number" {
		t.Fatalf("unexpected fields: %+v", p.Schema.Fields)
	}
	if !slices.Equal(p.EventTypes, []string{"OrderPlaced"}) {
		t.Fatalf("unexpected event types: %v", p.EventTypes)
	}
}

func TestLoadDirProjectionMixedRejected(t *testing.T) {
	dir := t.TempDir()
	writeFn(t, dir, "bad.js", `//@trigger projection p1 on E1
//@trigger http
//@schema c1 k:text
//@key k
function project(event) {}
`)
	rt := NewGojaRuntime(nil)
	if _, err := LoadDir(rt, nil, dir); err == nil {
		t.Fatal("expected projection-only error")
	}
}

func TestLoadDirProjectionMissingSchema(t *testing.T) {
	dir := t.TempDir()
	writeFn(t, dir, "bad.js", `//@trigger projection p1 on E1
function project(event) {}
`)
	rt := NewGojaRuntime(nil)
	if _, err := LoadDir(rt, nil, dir); err == nil {
		t.Fatal("expected missing schema error")
	}
}

func TestLoadDirMissing(t *testing.T) {
	rt := NewGojaRuntime(nil)
	loaded, err := LoadDir(rt, nil, filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.HTTP.Names()) != 0 || len(loaded.Projections) != 0 || len(loaded.Deciders) != 0 {
		t.Fatal("expected empty results")
	}
}

func TestParseTriggers(t *testing.T) {
	tr, err := parseTriggers("//@trigger event A B\n//@trigger http\nconsole.log(1)\n")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tr.eventTypes, []string{"A", "B"}) || !tr.isHTTP {
		t.Fatalf("got %+v", tr)
	}

	tr, err = parseTriggers("//@trigger projection p on E1 E2\n//@schema c k:text\n//@key k\n")
	if err != nil {
		t.Fatal(err)
	}
	if tr.projection != "p" || !slices.Equal(tr.projectionOn, []string{"E1", "E2"}) || tr.schemaRaw != "c k:text" || tr.key != "k" {
		t.Fatalf("got %+v", tr)
	}

	// cron schedule keeps its spaces
	tr, err = parseTriggers("//@trigger cron */5 * * * *\n")
	if err != nil {
		t.Fatal(err)
	}
	if tr.cron != "*/5 * * * *" {
		t.Fatalf("unexpected cron schedule: %q", tr.cron)
	}

	// duplicate cron rejected
	if _, err = parseTriggers("//@trigger cron * * * * *\n//@trigger cron * * * * *\n"); err == nil {
		t.Fatal("expected duplicate cron error")
	}

	// directives must lead the file
	tr, err = parseTriggers("console.log(1)\n//@trigger event A\n")
	if err != nil {
		t.Fatal(err)
	}
	if !tr.empty() {
		t.Fatalf("late directives should be ignored, got %+v", tr)
	}

	// malformed projection directive
	if _, err = parseTriggers("//@trigger projection p E1\n"); err == nil {
		t.Fatal("expected malformed projection directive error")
	}
}
