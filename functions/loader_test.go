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
	if len(p.Schemas) != 1 || p.Schemas[0].Collection != "orders_by_customer" || p.Schemas[0].Key != "customerRef" {
		t.Fatalf("unexpected schemas: %+v", p.Schemas)
	}
	if len(p.Schemas[0].Fields) != 2 || p.Schemas[0].Fields[1].Type != "number" {
		t.Fatalf("unexpected fields: %+v", p.Schemas[0].Fields)
	}
	if !slices.Equal(p.EventTypes, []string{"OrderPlaced"}) {
		t.Fatalf("unexpected event types: %v", p.EventTypes)
	}
}

func TestLoadDirProjectionMultiSchema(t *testing.T) {
	dir := t.TempDir()
	writeFn(t, dir, "rollup.js", `//@trigger projection sales on SalePlaced
//@schema customers cust:text total:number
//@key cust
//@schema products sku:text sold:number
//@key sku
function project(event) { return; }
`)

	rt := NewGojaRuntime(nil)
	loaded, err := LoadDir(rt, nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Projections) != 1 {
		t.Fatalf("expected 1 projection, got %d", len(loaded.Projections))
	}
	p := loaded.Projections[0]
	if len(p.Schemas) != 2 {
		t.Fatalf("expected 2 schemas, got %+v", p.Schemas)
	}
	if p.Schemas[0].Collection != "customers" || p.Schemas[0].Key != "cust" ||
		p.Schemas[1].Collection != "products" || p.Schemas[1].Key != "sku" {
		t.Fatalf("keys mis-paired: %+v", p.Schemas)
	}
	if got := p.Collections(); !slices.Equal(got, []string{"customers", "products"}) {
		t.Fatalf("unexpected collections: %v", got)
	}
}

func TestLoadDirProjectionSchemaKeyPairingRejections(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"key before schema", "//@trigger projection p on E1\n//@key k\n//@schema c k:text\nfunction project(event) {}\n"},
		{"duplicate key for one schema", "//@trigger projection p on E1\n//@schema c k:text\n//@key k\n//@key k\nfunction project(event) {}\n"},
		{"second schema missing key", "//@trigger projection p on E1\n//@schema c k:text\n//@key k\n//@schema d x:number\nfunction project(event) {}\n"},
		{"duplicate collection", "//@trigger projection p on E1\n//@schema c k:text\n//@key k\n//@schema c x:number\n//@key x\nfunction project(event) {}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFn(t, dir, "bad.js", tc.src)
			rt := NewGojaRuntime(nil)
			if _, err := LoadDir(rt, nil, dir); err == nil {
				t.Fatal("expected rejection")
			}
		})
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
	if tr.projection != "p" || !slices.Equal(tr.projectionOn, []string{"E1", "E2"}) ||
		len(tr.schemas) != 1 || tr.schemas[0].raw != "c k:text" || tr.schemas[0].key != "k" {
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

func TestParseTriggersRule(t *testing.T) {
	tr, err := parseTriggers("//@rule tickets authenticated\n")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tr.rules["tickets"], "authenticated"; got != want {
		t.Fatalf("rules[tickets] = %q, want %q", got, want)
	}

	// a raw rule expression keeps its internal spaces
	tr, err = parseTriggers(`//@rule tickets @request.auth.id != ""` + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tr.rules["tickets"], `@request.auth.id != ""`; got != want {
		t.Fatalf("rules[tickets] = %q, want %q", got, want)
	}

	cases := []struct {
		name string
		src  string
	}{
		{"missing value", "//@rule tickets\n"},
		{"missing collection and value", "//@rule\n"},
		{"invalid collection name", "//@rule 1tickets public\n"},
		{"duplicate rule for one collection", "//@rule tickets public\n//@rule tickets authenticated\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseTriggers(tc.src); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestLoadDirProjectionRule(t *testing.T) {
	dir := t.TempDir()
	writeFn(t, dir, "rollup.js", `//@trigger projection tickets_by_status on TicketOpened
//@schema tickets_by_status ticketId:text status:text
//@key ticketId
//@rule tickets_by_status authenticated
function project(event) { return; }
`)

	rt := NewGojaRuntime(nil)
	loaded, err := LoadDir(rt, nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	p := loaded.Projections[0]
	if p.Schemas[0].RuleOverride == nil || *p.Schemas[0].RuleOverride != "authenticated" {
		t.Fatalf("unexpected rule override: %+v", p.Schemas[0].RuleOverride)
	}
}

func TestLoadDirProjectionRuleUnmatchedCollectionRejected(t *testing.T) {
	dir := t.TempDir()
	writeFn(t, dir, "bad.js", `//@trigger projection tickets on TicketOpened
//@schema tickets ticketId:text
//@key ticketId
//@rule other_collection authenticated
function project(event) { return; }
`)
	rt := NewGojaRuntime(nil)
	if _, err := LoadDir(rt, nil, dir); err == nil {
		t.Fatal("expected rejection: //@rule names a collection with no //@schema")
	}
}
