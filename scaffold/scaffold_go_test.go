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

// finding3Domain exercises every Finding 3 shape in one aggregate: an
// endsStream event resetting Exists, a toggle derivation, a count
// derivation with an explicit rowKeyField (cross-stream events, the
// staffCount case), a sum derivation, and a scoped read model with no
// derivation at all (the flagged-entries case) -- proposal Examples 1-4.
func finding3Domain() Domain {
	return Domain{
		Aggregate: "staff",
		Commands: []Command{
			{Name: "CreateStaff", Once: true,
				Fields: []Field{{Name: "name", Type: "text"}},
				Events: []Event{{Name: "StaffCreated", Fields: []Field{{Name: "name", Type: "text"}}}}},
			{Name: "EnableSso", RequiresExisting: true,
				Events: []Event{{Name: "StaffSsoEnabled", NoFields: true}}},
			{Name: "DisableSso", RequiresExisting: true,
				Events: []Event{{Name: "StaffSsoDisabled", NoFields: true}}},
			{Name: "RemoveStaff", RequiresExisting: true,
				Events: []Event{{Name: "StaffRemoved", NoFields: true, EndsStream: true}}},
		},
		ReadModels: []ReadModel{
			{
				Collection: "staffRoster",
				Key:        "staffId",
				Fields: []Field{
					{Name: "name", Type: "text"},
					{Name: "ssoEnabled", Type: "bool", Derivation: &FieldDerivation{
						Kind: DerivationToggle, OnEventIDs: []string{"StaffSsoEnabled"}, OffEventIDs: []string{"StaffSsoDisabled"},
					}},
				},
				On: []string{"StaffCreated", "StaffSsoEnabled", "StaffSsoDisabled"},
			},
			{
				Collection: "projects",
				Key:        "projectId",
				Fields: []Field{
					{Name: "staffCount", Type: "number", Derivation: &FieldDerivation{
						Kind: DerivationCount, IncrementOnEventIDs: []string{"StaffAssignedToProject"},
						DecrementOnEventIDs: []string{"StaffUnassignedFromProject"}, RowKeyField: "projectId",
					}},
					{Name: "totalHours", Type: "number", Derivation: &FieldDerivation{
						Kind: DerivationSum, AddOnEventIDs: []string{"TimeLogged"}, SubtractOnEventIDs: []string{"TimeReversed"},
						AmountField: "hours", RowKeyField: "projectId",
					}},
				},
				On: []string{"StaffAssignedToProject", "StaffUnassignedFromProject", "TimeLogged", "TimeReversed"},
			},
			{
				Collection: "flaggedEntries",
				Key:        "entryId",
				Fields:     []Field{{Name: "projectId", Type: "text"}},
				On:         []string{"EntryFlagged"},
				Scopes: []ReadModelScope{{
					Param: "pmStaffId",
					Via: ReadModelScopeVia{
						Collection: "projectManagers", MatchParamTo: "staffId",
						SelectField: "projectId", FilterLocalField: "projectId",
					},
				}},
			},
		},
	}
}

// TestFinding3EndsStreamResetsExists: an endsStream event must emit
// `s.Exists = false` in Evolve, not the unconditional `true` every other
// event gets.
func TestFinding3EndsStreamResetsExists(t *testing.T) {
	files, err := finding3Domain().GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	dec := files[0].Source
	if !strings.Contains(dec, "case StaffRemoved:\n\t\t\t\ts.Exists = false") {
		t.Errorf("expected StaffRemoved to reset Exists to false:\n%s", dec)
	}
	if !strings.Contains(dec, "case StaffCreated:") || strings.Contains(dec, "case StaffCreated:\n\t\t\t\ts.Exists = false") {
		t.Errorf("a non-endsStream event must still set Exists true:\n%s", dec)
	}
}

// TestFinding3ProjectionDerivations: the projection for a toggle + a
// cross-stream count + a sum must route each trigger event to its own
// dedicated helper, not the generic merge, and getOrCreate must seed every
// derived field's initial value on a brand new row.
func TestFinding3ProjectionDerivations(t *testing.T) {
	files, err := finding3Domain().GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	roster := files[1].Source // staffRoster
	for _, want := range []string{
		`case "StaffSsoEnabled":`,
		`return p.setToggle(ctx, ev.AggregateID, "ssoEnabled", true)`,
		`case "StaffSsoDisabled":`,
		`return p.setToggle(ctx, ev.AggregateID, "ssoEnabled", false)`,
		`case "StaffCreated":`,
		"return p.applyMerge(ctx, ev)",
		`rec.Set("ssoEnabled", false)`, // Initial default seeded on create
	} {
		if !strings.Contains(roster, want) {
			t.Errorf("staffRoster projection missing %q:\n%s", want, roster)
		}
	}

	projects := files[2].Source // projects
	for _, want := range []string{
		`case "StaffAssignedToProject":`,
		`return p.bumpCount(ctx, ev, "staffCount", "projectId", 1)`,
		`case "StaffUnassignedFromProject":`,
		`return p.bumpCount(ctx, ev, "staffCount", "projectId", -1)`,
		`case "TimeLogged":`,
		`return p.bumpSum(ctx, ev, "totalHours", "hours", "projectId", 1)`,
		`case "TimeReversed":`,
		`return p.bumpSum(ctx, ev, "totalHours", "hours", "projectId", -1)`,
		`rec.Set("staffCount", 0)`,
		`rec.Set("totalHours", 0.0)`,
	} {
		if !strings.Contains(projects, want) {
			t.Errorf("projects projection missing %q:\n%s", want, projects)
		}
	}
	if strings.Contains(projects, "applyMerge") {
		t.Errorf("projects has no plain merge events; applyMerge must not be generated:\n%s", projects)
	}
}

// TestFinding3ScopeResolver: a read model with a declared scope, even with
// no derivation at all, gets a Resolve<Param> helper querying the via
// collection.
func TestFinding3ScopeResolver(t *testing.T) {
	files, err := finding3Domain().GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	flagged := files[3].Source
	for _, want := range []string{
		"func (p *FlaggedEntriesProjection) ResolvePmStaffId(value string) ([]string, error) {",
		`p.app.FindRecordsByFilter("projectManagers", "staffId = {:v}", "", -1, 0, map[string]any{"v": value})`,
		`ids = append(ids, r.GetString("projectId"))`,
	} {
		if !strings.Contains(flagged, want) {
			t.Errorf("flaggedEntries projection missing %q:\n%s", want, flagged)
		}
	}
}

// TestFinding3DomainCompiles proves the whole Finding 3 domain -- endsStream,
// toggle, count, sum and a scope resolver together -- is not just
// syntactically valid but actually compiles against the real pocketcqrs
// packages, the same way TestGeneratedGoDomainCompiles proves the plain path.
func TestFinding3DomainCompiles(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	files, err := finding3Domain().GenerateGo()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	goMod := "module finding3-domain-smoke\n\ngo 1.25.0\n\n" +
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
		t.Fatalf("Finding 3 domain failed to build:\n%s", out)
	}
}

// finding4Domain exercises F-20/schema 2.3.0's groupBy derivation: a
// payrollPeriods read model whose staffTotals field is a nested list of
// {staffId, outOfHoursHours, entriesLogged} rows, one per distinct staff
// member who logged time in the period — outOfHoursHours a sum subfield,
// entriesLogged a count subfield, so both groupBy-owned kinds are covered
// in one model, mirroring the real project/gotimesheets shape this closes
// the gap for (FAULTS-AND-WORK.md F-20).
func finding4Domain() Domain {
	return Domain{
		Aggregate: "payrollPeriod",
		Commands: []Command{
			{Name: "CreatePayrollPeriod", Once: true,
				Events: []Event{{Name: "PayrollPeriodCreated", NoFields: true}}},
			{Name: "LogTime", RequiresExisting: true,
				Events: []Event{{Name: "TimeLogged", Fields: []Field{
					{Name: "staffId", Type: "text"}, {Name: "hours", Type: "number"},
				}}}},
		},
		ReadModels: []ReadModel{
			{
				Collection: "payrollPeriods",
				Key:        "periodId",
				Fields: []Field{
					{Name: "staffTotals", Type: "json",
						Derivation: &FieldDerivation{Kind: DerivationGroupBy, GroupByField: "staffId"},
						Subfields: []Field{
							{Name: "staffId", Type: "text"},
							{Name: "outOfHoursHours", Type: "number", Derivation: &FieldDerivation{
								Kind: DerivationSum, AddOnEventIDs: []string{"TimeLogged"},
								AmountField: "hours", RowKeyField: "periodId",
							}},
							{Name: "entriesLogged", Type: "number", Derivation: &FieldDerivation{
								Kind: DerivationCount, IncrementOnEventIDs: []string{"TimeLogged"}, RowKeyField: "periodId",
							}},
						},
					},
				},
				On: []string{"TimeLogged"},
			},
		},
	}
}

// TestFinding4ProjectionDerivations: the groupBy field's own trigger event
// must route to BOTH its subfields' dedicated helpers (sum then count, in
// declaration order), never the generic merge, and getOrCreate must seed
// the parent field's own zero-state (an empty list) on a brand new row.
func TestFinding4ProjectionDerivations(t *testing.T) {
	files, err := finding4Domain().GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	periods := files[1].Source // payrollPeriods
	for _, want := range []string{
		`case "TimeLogged":`,
		`p.bumpGroupBySum(ctx, ev, "staffTotals", "staffId", "outOfHoursHours", "hours", "periodId", 1)`,
		`return p.bumpGroupByCount(ctx, ev, "staffTotals", "staffId", "entriesLogged", "periodId", 1)`,
		`rec.Set("staffTotals", []any{})`, // zero-state default seeded on create
		"func (p *PayrollPeriodsProjection) groupByEntry(rec *core.Record, field, groupKeyField string, groupKeyValue any) (map[string]any, []map[string]any) {",
		"_ = rec.UnmarshalJSONField(field, &rows)",
		"if entry[groupKeyField] == groupKeyValue {",
		"func (p *PayrollPeriodsProjection) bumpGroupBySum(ctx context.Context, ev events.Event, field, groupKeyField, subfield, amountField, rowKeyField string, sign float64) error {",
		"func (p *PayrollPeriodsProjection) bumpGroupByCount(ctx context.Context, ev events.Event, field, groupKeyField, subfield, rowKeyField string, delta int) error {",
	} {
		if !strings.Contains(periods, want) {
			t.Errorf("payrollPeriods projection missing %q:\n%s", want, periods)
		}
	}
	if strings.Contains(periods, "applyMerge") {
		// PayrollPeriodCreated has no derived-field trigger of its own AND is
		// not otherwise merged (staffTotals is the read model's only field),
		// so there is nothing left for the generic merge path to do.
		t.Errorf("payrollPeriods has no plain merge events; applyMerge must not be generated:\n%s", periods)
	}
}

// TestFinding4DomainCompiles proves the whole groupBy domain actually
// compiles against the real pocketcqrs packages, the same way
// TestFinding3DomainCompiles proves toggle/count/sum.
func TestFinding4DomainCompiles(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	files, err := finding4Domain().GenerateGo()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	goMod := "module finding4-domain-smoke\n\ngo 1.25.0\n\n" +
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
		t.Fatalf("Finding 4 domain failed to build:\n%s", out)
	}
}
