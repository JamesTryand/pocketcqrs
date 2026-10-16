package emschema

import (
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/scaffold"
)

// requiredRoleDocument builds a minimal EventModeling document exercising
// schema 2.7.0's readModel.requiredRole: a member-directory read model gated
// to "supervisor"/"administrator", the read-side mirror of the fixture
// TestBuildPolicies-style tests use for command.requiredRole (see the
// authorize package's own tests), mirroring dotnetcqrs's own port
// (commit 382fb9e, v0.8.0), raised because an unscoped read model returned its full tenant-wide contents to any
// signed-in user.
func requiredRoleDocument(roles ...string) *Document {
	return &Document{
		EventModelingSchemaVersion: SchemaVersion,
		ID:                         "d1",
		Name:                       "Member Directory RequiredRole",
		Swimlanes:                  []Swimlane{{ID: "sys", Name: "System", Kind: "system"}},
		Events: map[string]Event{
			"member-joined": {
				Name: "Member Joined", SwimlaneID: "sys", Aggregate: "Member",
				Fields: []Field{
					{Name: "memberId", Type: "string", IDAttribute: true},
					{Name: "annualFee", Type: "number"},
				},
			},
		},
		Commands: map[string]Command{
			"add-member": {
				Name: "Add Member", Aggregate: "Member",
				Fields: []Field{
					{Name: "memberId", Type: "string"},
					{Name: "annualFee", Type: "number"},
				},
			},
		},
		ReadModels: map[string]ReadModel{
			"member-directory": {
				Name:              "Member Directory",
				BuiltFromEventIDs: []string{"member-joined"},
				Fields: []Field{
					{Name: "memberId", Type: "string", IDAttribute: true},
					{Name: "annualFee", Type: "number"},
				},
				RequiredRole: roles,
			},
		},
		Screens: map[string]Screen{
			"scr-add":       {Name: "Add Member Screen"},
			"scr-directory": {Name: "View Member Directory"},
		},
		Slices: []Slice{
			{
				ID: "add-member-slice", Name: "Add Member", Pattern: PatternStateChange,
				SwimlaneID: "sys", Status: "wireframe", ScreenID: "scr-add",
				CommandID: "add-member", EventIDs: []string{"member-joined"},
			},
			{
				ID: "view-member-directory-slice", Name: "View Member Directory", Pattern: PatternStateView,
				SwimlaneID: "sys", Status: "wireframe", ScreenID: "scr-directory",
				ReadModelID: "member-directory",
			},
		},
	}
}

// TestMapCarriesReadModelRequiredRole proves Map() carries a declared
// readModel.requiredRole through to scaffold.ReadModel, the same
// round-trip proof TestImportAndVerifyDateRangeFilter established for
// Filters.
func TestMapCarriesReadModelRequiredRole(t *testing.T) {
	doc := requiredRoleDocument("supervisor", "administrator")

	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatalf("expected the document to map cleanly: %v", err)
	}
	if len(mapped.Domains) != 1 {
		t.Fatalf("expected one aggregate, got %d", len(mapped.Domains))
	}
	rm := mapped.Domains[0].ReadModels[0]
	if len(rm.RequiredRole) != 2 || rm.RequiredRole[0] != "supervisor" || rm.RequiredRole[1] != "administrator" {
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
	doc := requiredRoleDocument("supervisor", "administrator")
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
		if f.Name == "memberDirectory.js" {
			projection = f.Source
		}
	}
	if projection == "" {
		t.Fatalf("expected a memberDirectory.js projection file, got files: %+v", fileNames(files))
	}
	wantRule := `//@rule memberDirectory @request.auth.role = "supervisor" || @request.auth.role = "administrator"`
	if !strings.Contains(projection, wantRule) {
		t.Fatalf("expected the projection to declare a role rule.\nwant substring: %s\ngot:\n%s", wantRule, projection)
	}
}

// A read model declaring no access rule is read by every signed-in caller
// (schema 3.8.0), so the importer gives it "authenticated" rather than the
// deployment default, which is public unless set.
func TestGeneratedProjectionIsSignedInOnlyWhenNoRuleDeclared(t *testing.T) {
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
		if f.Name == "memberDirectory.js" && !strings.Contains(f.Source, "//@rule memberDirectory authenticated\n") {
			t.Fatalf("expected //@rule memberDirectory authenticated, got:\n%s", f.Source)
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
