package emschema

import (
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/scaffold"
)

// requiredRoleDocument builds a minimal EventModeling document exercising
// schema 2.7.0's readModel.requiredRole: a staff-roster read model gated to
// "manager"/"administrator", the read-side mirror of the fixture
// TestBuildPolicies-style tests use for command.requiredRole (see the
// authorize package's own tests), mirroring dotnetcqrs's own port
// (commit 382fb9e, v0.8.0), raised by project/timesheets's D12 — an
// unscoped read model returned its full tenant-wide contents to any
// signed-in user.
func requiredRoleDocument(roles ...string) *Document {
	return &Document{
		EventModelingSchemaVersion: SchemaVersion,
		ID:                         "d1",
		Name:                       "Staff Roster RequiredRole",
		Swimlanes:                  []Swimlane{{ID: "sys", Name: "System", Kind: "system"}},
		Events: map[string]Event{
			"staff-onboarded": {
				Name: "Staff Onboarded", SwimlaneID: "sys", Aggregate: "Staff",
				Fields: []Field{
					{Name: "staffId", Type: "string", IDAttribute: true},
					{Name: "hourlyCost", Type: "number"},
				},
			},
		},
		Commands: map[string]Command{
			"onboard-staff": {
				Name: "Onboard Staff", Aggregate: "Staff",
				Fields: []Field{
					{Name: "staffId", Type: "string"},
					{Name: "hourlyCost", Type: "number"},
				},
			},
		},
		ReadModels: map[string]ReadModel{
			"staff-roster": {
				Name:              "Staff Roster",
				BuiltFromEventIDs: []string{"staff-onboarded"},
				Fields: []Field{
					{Name: "staffId", Type: "string", IDAttribute: true},
					{Name: "hourlyCost", Type: "number"},
				},
				RequiredRole: roles,
			},
		},
		Screens: map[string]Screen{
			"scr-onboard": {Name: "Onboard Staff Screen"},
			"scr-roster":  {Name: "View Staff Roster"},
		},
		Slices: []Slice{
			{
				ID: "onboard-staff-slice", Name: "Onboard Staff", Pattern: PatternStateChange,
				SwimlaneID: "sys", Status: "wireframe", ScreenID: "scr-onboard",
				CommandID: "onboard-staff", EventIDs: []string{"staff-onboarded"},
			},
			{
				ID: "view-staff-roster-slice", Name: "View Staff Roster", Pattern: PatternStateView,
				SwimlaneID: "sys", Status: "wireframe", ScreenID: "scr-roster",
				ReadModelID: "staff-roster",
			},
		},
	}
}

// TestMapCarriesReadModelRequiredRole proves Map() carries a declared
// readModel.requiredRole through to scaffold.ReadModel, the same
// round-trip proof TestImportAndVerifyDateRangeFilter established for
// Filters.
func TestMapCarriesReadModelRequiredRole(t *testing.T) {
	doc := requiredRoleDocument("manager", "administrator")

	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatalf("expected the document to map cleanly: %v", err)
	}
	if len(mapped.Domains) != 1 {
		t.Fatalf("expected one aggregate, got %d", len(mapped.Domains))
	}
	rm := mapped.Domains[0].ReadModels[0]
	if len(rm.RequiredRole) != 2 || rm.RequiredRole[0] != "manager" || rm.RequiredRole[1] != "administrator" {
		t.Fatalf("requiredRole not carried through correctly: %+v", rm.RequiredRole)
	}
}

// TestGeneratedProjectionEmitsRoleRule proves the generated JS projection
// actually declares a //@rule directive gating the collection to the
// required role(s) — the load-bearing check, since this is the only real
// enforcement point PocketBase's own collection API gives reads (no
// generated route to hook a check into, unlike dotnetcqrs's
// resolveOwnRole).
func TestGeneratedProjectionEmitsRoleRule(t *testing.T) {
	doc := requiredRoleDocument("manager", "administrator")
	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatalf("expected the document to map cleanly: %v", err)
	}
	files, err := mapped.Domains[0].Generate()
	if err != nil {
		t.Fatalf("expected generation to succeed: %v", err)
	}
	var projection string
	for _, f := range files {
		if f.Name == "staffRoster.js" {
			projection = f.Source
		}
	}
	if projection == "" {
		t.Fatalf("expected a staffRoster.js projection file, got files: %+v", fileNames(files))
	}
	wantRule := `//@rule staffRoster @request.auth.role = "manager" || @request.auth.role = "administrator"`
	if !strings.Contains(projection, wantRule) {
		t.Fatalf("expected the projection to declare a role rule.\nwant substring: %s\ngot:\n%s", wantRule, projection)
	}
}

// TestGeneratedProjectionOmitsRuleWhenNoRoleRequired proves the absence of
// requiredRole leaves the generated file exactly as before this capability
// existed — no //@rule line at all, same "no vacuous default" posture
// dotnetcqrs's own unwired resolveOwnRole takes.
func TestGeneratedProjectionOmitsRuleWhenNoRoleRequired(t *testing.T) {
	doc := requiredRoleDocument() // no roles
	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatalf("expected the document to map cleanly: %v", err)
	}
	files, err := mapped.Domains[0].Generate()
	if err != nil {
		t.Fatalf("expected generation to succeed: %v", err)
	}
	for _, f := range files {
		if f.Name == "staffRoster.js" && strings.Contains(f.Source, "@rule") {
			t.Fatalf("expected no //@rule directive when requiredRole is absent, got:\n%s", f.Source)
		}
	}
}

func fileNames(files []scaffold.File) []string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	return names
}
