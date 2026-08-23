package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGenerateGoShape: the generated Go files must declare what the model
// said, in the shapes docs/go-guide.md's JS→Go table promises.
func TestGenerateGoShape(t *testing.T) {
	d := sampleDomain()
	d.Reactors = []Reactor{{
		Name: "autoClose", On: []string{"TicketOpened"},
		Aggregate: "audit", Command: "Record", IDPrefix: "audit-",
	}}
	files, err := d.GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("expected a decider, a projection and a reactor, got %d files", len(files))
	}

	dec, proj, react := files[0], files[1], files[2]
	if dec.Name != "ticket.go" {
		t.Errorf("decider file name: %q", dec.Name)
	}
	if proj.Name != "tickets.go" {
		t.Errorf("projection file name: %q", proj.Name)
	}
	if react.Name != "autoClose.go" {
		t.Errorf("reactor file name: %q", react.Name)
	}

	for _, want := range []string{
		"package ticket",
		`TicketOpened = "TicketOpened"`,
		`TicketClosed = "TicketClosed"`,
		"type State struct {",
		"Subject  string  `json:\"subject\"`", // gofmt-aligned
		"func Ticket() *decider.Decider[State] {",
		`case "OpenTicket":`,
		"already exists",
		"does not exist",
		"func(s State, ev events.Event) (State, error) {",
		"decider.Register(registry, \"ticket\", Ticket())",
	} {
		if !strings.Contains(dec.Source, want) {
			t.Errorf("decider source missing %q:\n%s", want, dec.Source)
		}
	}

	for _, want := range []string{
		"package ticket",
		`func NewTickets(app core.App) *TicketsProjection`,
		`func (p *TicketsProjection) Collections() []string { return []string{"tickets"} }`,
		`case "TicketOpened", "TicketClosed":`,
		"var data map[string]any",
		`p.app.FindFirstRecordByData("tickets", "ticketId", ev.AggregateID)`,
	} {
		if !strings.Contains(proj.Source, want) {
			t.Errorf("projection source missing %q:\n%s", want, proj.Source)
		}
	}

	for _, want := range []string{
		"package ticket",
		"func AutoClose() reactors.Reactor",
		`func (autoCloseReactor) Name() string { return "autoClose" }`,
		`case "TicketOpened":`,
		`Aggregate: "audit"`,
		`ID:        "audit-" + ev.AggregateID`,
		`Command:   decider.Command{Name: "Record", Payload: ev.Data}`,
	} {
		if !strings.Contains(react.Source, want) {
			t.Errorf("reactor source missing %q:\n%s", want, react.Source)
		}
	}
}

// TestGenerateGoValidatesFirst: GenerateGo shares Validate() with Generate()
// — an invalid model must not silently produce Go source.
func TestGenerateGoValidatesFirst(t *testing.T) {
	d := Domain{Aggregate: "9bad"}
	if _, err := d.GenerateGo(); err == nil {
		t.Fatal("expected an invalid model to be refused")
	}
}

// TestGenerateGoWriteOnlyDomain: a slice with no read models or reactors
// generates only a decider, mirroring Generate()'s own JS behavior.
func TestGenerateGoWriteOnlyDomain(t *testing.T) {
	d := Domain{
		Aggregate: "audit",
		Commands:  []Command{{Name: "Record", Events: []Event{{Name: "Recorded", NoFields: true}}}},
	}
	files, err := d.GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected only a decider, got %d files", len(files))
	}
	if !strings.Contains(files[0].Source, `Data: json.RawMessage("{}")`) {
		t.Errorf("a fieldless event must encode an empty object:\n%s", files[0].Source)
	}
}

// TestGoPackageNameLowercasesOnlyFirstRune: Go package names are
// conventionally all-lowercase, but the aggregate's own recorded identity
// (the registration string, file names) must not change because of it.
func TestGoPackageNameLowercasesOnlyFirstRune(t *testing.T) {
	d := Domain{
		Aggregate: "SupportTicket",
		Commands:  []Command{{Name: "Open", Once: true, Events: []Event{{Name: "Opened", NoFields: true}}}},
	}
	files, err := d.GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(files[0].Source, "package supportTicket\n") {
		t.Errorf("expected the package clause to lower-case only the first rune:\n%s", files[0].Source)
	}
	if !strings.Contains(files[0].Source, `decider.Register(registry, "SupportTicket", SupportTicket())`) {
		t.Errorf("the aggregate's own name must stay verbatim in the registration line:\n%s", files[0].Source)
	}
	if files[0].Name != "SupportTicket.go" {
		t.Errorf("the file name must stay verbatim too, got %q", files[0].Name)
	}
}

// TestFieldTypeConflictIsWarnedAndResolvedConsistently: a field name
// declared with two different //@schema types across events is a real
// model ambiguity. It must be named in Warnings() (the existing mechanism
// for "unfinished, not broken") and the generated code must still compile
// — first type wins, used everywhere that name appears.
func TestFieldTypeConflictIsWarnedAndResolvedConsistently(t *testing.T) {
	d := Domain{
		Aggregate: "payment",
		Commands: []Command{
			{Name: "Authorize", Once: true,
				Events: []Event{{Name: "Authorized", Fields: []Field{{Name: "amount", Type: "number"}}}}},
			{Name: "Refund", RequiresExisting: true,
				Events: []Event{{Name: "Refunded", Fields: []Field{{Name: "amount", Type: "text"}}}}},
		},
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("a type conflict is not a hard error: %v", err)
	}

	var found bool
	for _, w := range d.Warnings() {
		if strings.Contains(w, `"amount"`) && strings.Contains(w, "number") && strings.Contains(w, "text") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a warning naming the amount field's conflicting types, got %v", d.Warnings())
	}

	files, err := d.GenerateGo()
	if err != nil {
		t.Fatalf("a type conflict must still generate valid Go: %v", err)
	}
	src := files[0].Source
	// first-seen (float64, from Authorized) used everywhere "amount"
	// appears: State, both commands' payload structs, both events' Evolve
	// data structs -- five occurrences, none of them the conflicting type.
	if got := strings.Count(src, "Amount float64"); got != 5 {
		t.Errorf("expected the first-seen type (float64) used consistently everywhere (5 places), got %d:\n%s", got, src)
	}
	if strings.Contains(src, "Amount string") {
		t.Errorf("the second event's own type must not leak into the generated struct:\n%s", src)
	}
}

// TestProjectionOnForeignEventUsesLiteralNotConstant: a read model's On may
// legitimately name another aggregate's events (Warnings' own "intended if
// it folds another aggregate's events" case) — the generated switch must
// use string literals, not a same-package constant that would not exist
// for a foreign event name.
func TestProjectionOnForeignEventUsesLiteralNotConstant(t *testing.T) {
	d := Domain{
		Aggregate: "audit",
		Commands:  []Command{{Name: "Record", Events: []Event{{Name: "Recorded", NoFields: true}}}},
		ReadModels: []ReadModel{{
			Collection: "auditLog", Key: "auditId",
			On: []string{"OrderPlaced"}, // not one of this aggregate's own events
		}},
	}
	files, err := d.GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(files[1].Source, `case "OrderPlaced":`) {
		t.Errorf("expected a quoted literal for a foreign event name:\n%s", files[1].Source)
	}
}

// TestGeneratedGoDomainCompiles proves the generated files are not just
// syntactically valid (formatGo's go/format.Source pass already guarantees
// that) but actually compile against real pocketcqrs packages: it writes
// them into a throwaway module with a replace directive back to this
// checkout and runs `go build` as a subprocess — the only failure mode
// that really matters for a code generator. Runs fully offline (the
// replace directive means no network fetch is needed).
func TestGeneratedGoDomainCompiles(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	d := sampleDomain()
	d.Reactors = []Reactor{{
		Name: "autoClose", On: []string{"TicketOpened"},
		Aggregate: "audit", Command: "Record", IDPrefix: "audit-",
	}}
	files, err := d.GenerateGo()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	goMod := "module generated-domain-smoke\n\ngo 1.25.0\n\n" +
		"require github.com/jamestryand/pocketcqrs v0.0.0\n\n" +
		"replace github.com/jamestryand/pocketcqrs => " + repoRoot + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.Name), []byte(f.Source), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	tidy.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy failed: %v\n%s", err, out)
	}

	build := exec.Command("go", "build", "./...")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("generated domain failed to build:\n%s", out)
	}
}
