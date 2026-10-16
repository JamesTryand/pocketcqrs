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
	if len(files) != 4 {
		t.Fatalf("expected a decider, a projection, its read route and a reactor, got %d files", len(files))
	}

	dec, proj, query, react := files[0], files[1], files[2], files[3]
	if query.Name != "tickets_query.go" || !strings.Contains(query.Source, "var TicketsQuery = queryroute.Spec{") {
		t.Errorf("read route spec: %q: %s", query.Name, query.Source)
	}
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

// derivationDomain exercises every derivation shape in one aggregate: an
// endsStream event resetting Exists, a toggle derivation, a count
// derivation with an explicit rowKeyField (cross-stream events, the
// memberCount case), a sum derivation, and a scoped read model with no
// derivation at all (the escalated-tickets case).
func derivationDomain() Domain {
	return Domain{
		Aggregate: "member",
		Commands: []Command{
			{Name: "CreateMember", Once: true,
				Fields: []Field{{Name: "name", Type: "text"}},
				Events: []Event{{Name: "MemberCreated", Fields: []Field{{Name: "name", Type: "text"}}}}},
			{Name: "EnableSso", RequiresExisting: true,
				Events: []Event{{Name: "MemberSsoEnabled", NoFields: true}}},
			{Name: "DisableSso", RequiresExisting: true,
				Events: []Event{{Name: "MemberSsoDisabled", NoFields: true}}},
			{Name: "RemoveMember", RequiresExisting: true,
				Events: []Event{{Name: "MemberRemoved", NoFields: true, EndsStream: true}}},
		},
		ReadModels: []ReadModel{
			{
				Collection: "memberDirectory",
				Key:        "memberId",
				// removing a member takes their row away (schema 3.9.0)
				RemovedBy: []string{"MemberRemoved"},
				Fields: []Field{
					{Name: "name", Type: "text"},
					{Name: "ssoEnabled", Type: "bool", Derivation: &FieldDerivation{
						Kind: DerivationToggle, OnEventIDs: []string{"MemberSsoEnabled"}, OffEventIDs: []string{"MemberSsoDisabled"},
					}},
				},
				On: []string{"MemberCreated", "MemberSsoEnabled", "MemberSsoDisabled", "MemberRemoved"},
			},
			{
				Collection: "teams",
				Key:        "teamId",
				Fields: []Field{
					{Name: "memberCount", Type: "number", Derivation: &FieldDerivation{
						Kind: DerivationCount, IncrementOnEventIDs: []string{"MemberAssignedToTeam"},
						DecrementOnEventIDs: []string{"MemberUnassignedFromTeam"}, RowKeyField: "teamId",
					}},
					{Name: "totalUnits", Type: "number", Derivation: &FieldDerivation{
						Kind: DerivationSum, AddOnEventIDs: []string{"SaleRecorded"}, SubtractOnEventIDs: []string{"SaleReversed"},
						AmountField: "units", RowKeyField: "teamId",
					}},
				},
				On: []string{"MemberAssignedToTeam", "MemberUnassignedFromTeam", "SaleRecorded", "SaleReversed"},
			},
			{
				Collection: "escalatedTickets",
				Key:        "ticketId",
				Fields:     []Field{{Name: "teamId", Type: "text"}},
				On:         []string{"TicketEscalated"},
				Scopes: []ReadModelScope{{
					Param: "leadMemberId",
					Via: ReadModelScopeVia{
						Collection: "teamLeads", MatchParamTo: "memberId",
						SelectField: "teamId", FilterLocalField: "teamId",
					},
				}},
			},
		},
	}
}

// A removal (schema 3.9.0) routes to remove ahead of the derived and
// merge cases, in both generators; TestDerivationDomainCompiles builds it.
func TestRemovalIsGenerated(t *testing.T) {
	goFiles, err := derivationDomain().GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	var directory string
	for _, f := range goFiles {
		if strings.Contains(f.Source, "type MemberDirectoryProjection struct") {
			directory = f.Source
		}
	}
	for _, want := range []string{
		"case \"MemberRemoved\":\n\t\treturn p.remove(ctx, ev.AggregateID)",
		"func (p *MemberDirectoryProjection) remove(ctx context.Context, rowKey string) error {",
	} {
		if !strings.Contains(directory, want) {
			t.Errorf("Go directory projection lacks %q:\n%s", want, directory)
		}
	}
	js, err := derivationDomain().Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js[1].Source, "if (['MemberRemoved'].includes(event.type)) { return [{ delete: event.aggregateId }]; }") {
		t.Errorf("JS directory projection lacks the removal:\n%s", js[1].Source)
	}
}

// TestDerivationEndsStreamResetsExists: an endsStream event must emit
// `s.Exists = false` in Evolve, not the unconditional `true` every other
// event gets.
func TestDerivationEndsStreamResetsExists(t *testing.T) {
	files, err := derivationDomain().GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	dec := files[0].Source
	if !strings.Contains(dec, "case MemberRemoved:\n\t\t\t\ts.Exists = false\n\t\t\t\ts.Ended = true") {
		t.Errorf("expected MemberRemoved to reset Exists to false and set Ended:\n%s", dec)
	}
	if !strings.Contains(dec, "\tEnded  bool\n") ||
		!strings.Contains(dec, "if s.Ended {\n\t\t\t\t\treturn nil, fmt.Errorf(\"member has ended and cannot begin again\")") {
		t.Errorf("expected an Ended flag that CreateMember refuses:\n%s", dec)
	}
	if !strings.Contains(dec, "case MemberCreated:") || strings.Contains(dec, "case MemberCreated:\n\t\t\t\ts.Exists = false") {
		t.Errorf("a non-endsStream event must still set Exists true:\n%s", dec)
	}
}

// TestDerivationProjectionDerivations: the projection for a toggle + a
// cross-stream count + a sum must route each trigger event to its own
// dedicated helper, not the generic merge, and getOrCreate must seed every
// derived field's initial value on a brand new row.
func TestDerivationProjectionDerivations(t *testing.T) {
	files, err := derivationDomain().GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	directory := files[1].Source // memberDirectory
	for _, want := range []string{
		`case "MemberSsoEnabled":`,
		`return p.setToggle(ctx, ev.AggregateID, "ssoEnabled", true)`,
		`case "MemberSsoDisabled":`,
		`return p.setToggle(ctx, ev.AggregateID, "ssoEnabled", false)`,
		`case "MemberCreated":`,
		"return p.applyMerge(ctx, ev)",
		`rec.Set("ssoEnabled", false)`, // Initial default seeded on create
	} {
		if !strings.Contains(directory, want) {
			t.Errorf("memberDirectory projection missing %q:\n%s", want, directory)
		}
	}

	teams := files[2].Source // teams
	for _, want := range []string{
		`case "MemberAssignedToTeam":`,
		`return p.bumpCount(ctx, ev, "memberCount", "teamId", 1)`,
		`case "MemberUnassignedFromTeam":`,
		`return p.bumpCount(ctx, ev, "memberCount", "teamId", -1)`,
		`case "SaleRecorded":`,
		`return p.bumpSum(ctx, ev, "totalUnits", "units", "teamId", 1)`,
		`case "SaleReversed":`,
		`return p.bumpSum(ctx, ev, "totalUnits", "units", "teamId", -1)`,
		`rec.Set("memberCount", 0)`,
		`rec.Set("totalUnits", 0.0)`,
	} {
		if !strings.Contains(teams, want) {
			t.Errorf("teams projection missing %q:\n%s", want, teams)
		}
	}
	if strings.Contains(teams, "applyMerge") {
		t.Errorf("teams has no plain merge events; applyMerge must not be generated:\n%s", teams)
	}
}

// TestDerivationScopeResolver: a read model with a declared scope, even with
// no derivation at all, gets a Resolve<Param> helper querying the via
// collection.
func TestDerivationScopeResolver(t *testing.T) {
	files, err := derivationDomain().GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	flagged := files[3].Source
	for _, want := range []string{
		"func (p *EscalatedTicketsProjection) ResolveLeadMemberId(value string) ([]string, error) {",
		`p.app.FindRecordsByFilter("teamLeads", "memberId = {:v}", "", -1, 0, map[string]any{"v": value})`,
		`ids = append(ids, r.GetString("teamId"))`,
	} {
		if !strings.Contains(flagged, want) {
			t.Errorf("escalatedTickets projection missing %q:\n%s", want, flagged)
		}
	}
}

// TestDerivationDomainCompiles proves the whole derivation domain -- endsStream,
// toggle, count, sum and a scope resolver together -- is not just
// syntactically valid but actually compiles against the real pocketcqrs
// packages, the same way TestGeneratedGoDomainCompiles proves the plain path.
func TestDerivationDomainCompiles(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	files, err := derivationDomain().GenerateGo()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	goMod := "module derivation-domain-smoke\n\ngo 1.25.0\n\n" +
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
		t.Fatalf("derivation domain failed to build:\n%s", out)
	}
}

// groupByDomain exercises schema 2.3.0's groupBy derivation: a
// salesPeriods read model whose memberTotals field is a nested list of
// {memberId, unitsSold, salesRecorded} rows, one per distinct member
// who recorded a sale in the period — unitsSold a sum subfield,
// salesRecorded a count subfield, so both groupBy-owned kinds are covered
// in one model.
func groupByDomain() Domain {
	return Domain{
		Aggregate: "salesPeriod",
		Commands: []Command{
			{Name: "CreateSalesPeriod", Once: true,
				Events: []Event{{Name: "SalesPeriodCreated", NoFields: true}}},
			{Name: "RecordSale", RequiresExisting: true,
				Events: []Event{{Name: "SaleRecorded", Fields: []Field{
					{Name: "memberId", Type: "text"}, {Name: "units", Type: "number"},
				}}}},
		},
		ReadModels: []ReadModel{
			{
				Collection: "salesPeriods",
				Key:        "periodId",
				Fields: []Field{
					{Name: "memberTotals", Type: "json",
						Derivation: &FieldDerivation{Kind: DerivationGroupBy, GroupByField: "memberId"},
						Subfields: []Field{
							{Name: "memberId", Type: "text"},
							{Name: "unitsSold", Type: "number", Derivation: &FieldDerivation{
								Kind: DerivationSum, AddOnEventIDs: []string{"SaleRecorded"},
								AmountField: "units", RowKeyField: "periodId",
							}},
							{Name: "salesRecorded", Type: "number", Derivation: &FieldDerivation{
								Kind: DerivationCount, IncrementOnEventIDs: []string{"SaleRecorded"}, RowKeyField: "periodId",
							}},
						},
					},
				},
				On: []string{"SaleRecorded"},
			},
		},
	}
}

// TestGroupByProjectionDerivations: the groupBy field's own trigger event
// must route to BOTH its subfields' dedicated helpers (sum then count, in
// declaration order), never the generic merge, and getOrCreate must seed
// the parent field's own zero-state (an empty list) on a brand new row.
func TestGroupByProjectionDerivations(t *testing.T) {
	files, err := groupByDomain().GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	periods := files[1].Source // salesPeriods
	for _, want := range []string{
		`case "SaleRecorded":`,
		`p.bumpGroupBySum(ctx, ev, "memberTotals", "memberId", "unitsSold", "units", "periodId", 1)`,
		`return p.bumpGroupByCount(ctx, ev, "memberTotals", "memberId", "salesRecorded", "periodId", 1)`,
		`rec.Set("memberTotals", []any{})`, // zero-state default seeded on create
		"func (p *SalesPeriodsProjection) groupByEntry(rec *core.Record, field, groupKeyField string, groupKeyValue any) (map[string]any, []map[string]any) {",
		"_ = rec.UnmarshalJSONField(field, &rows)",
		"if entry[groupKeyField] == groupKeyValue {",
		"func (p *SalesPeriodsProjection) bumpGroupBySum(ctx context.Context, ev events.Event, field, groupKeyField, subfield, amountField, rowKeyField string, sign float64) error {",
		"func (p *SalesPeriodsProjection) bumpGroupByCount(ctx context.Context, ev events.Event, field, groupKeyField, subfield, rowKeyField string, delta int) error {",
	} {
		if !strings.Contains(periods, want) {
			t.Errorf("salesPeriods projection missing %q:\n%s", want, periods)
		}
	}
	if strings.Contains(periods, "applyMerge") {
		// SalesPeriodCreated has no derived-field trigger of its own AND is
		// not otherwise merged (memberTotals is the read model's only field),
		// so there is nothing left for the generic merge path to do.
		t.Errorf("salesPeriods has no plain merge events; applyMerge must not be generated:\n%s", periods)
	}
}

// TestGroupByDomainCompiles proves the whole groupBy domain actually
// compiles against the real pocketcqrs packages, the same way
// TestDerivationDomainCompiles proves toggle/count/sum.
func TestGroupByDomainCompiles(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	files, err := groupByDomain().GenerateGo()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	goMod := "module groupby-domain-smoke\n\ngo 1.25.0\n\n" +
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
		t.Fatalf("groupBy domain failed to build:\n%s", out)
	}
}
