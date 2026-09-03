package emschema

import (
	"encoding/json"
	"strings"
	"testing"
)

// baseValidDoc returns the smallest document Lint accepts, so each test below
// only has to add the one command-authorization declaration it's checking.
func baseValidDoc() *Document {
	return &Document{
		EventModelingSchemaVersion: SchemaVersion,
		ID:                         "test",
		Name:                       "Test",
		Swimlanes:                  []Swimlane{{ID: "sys", Name: "System", Kind: "system"}},
		Events: map[string]Event{
			"thing-created": {Name: "Thing Created", SwimlaneID: "sys", Aggregate: "thing"},
		},
		Commands: map[string]Command{
			"create-thing": {Name: "Create Thing", Aggregate: "thing"},
		},
		ReadModels: map[string]ReadModel{
			"things": {
				Name:              "Things",
				BuiltFromEventIDs: []string{"thing-created"},
				Fields: []Field{
					{Name: "thingId", Type: "string", IDAttribute: true},
					{Name: "ownerId", Type: "string"},
					{Name: "projectId", Type: "string"},
				},
			},
			"project-managers": {
				Name:              "Project Managers",
				BuiltFromEventIDs: []string{"thing-created"},
				Fields: []Field{
					{Name: "assignmentId", Type: "string", IDAttribute: true},
					{Name: "projectId", Type: "string"},
					{Name: "staffId", Type: "string"},
				},
			},
		},
		Screens: map[string]Screen{"create-thing-screen": {Name: "Create Thing"}},
		Slices: []Slice{{
			ID: "create-thing-slice", Name: "Create Thing", Pattern: PatternStateChange,
			SwimlaneID: "sys", Status: "informational", ScreenID: "create-thing-screen",
			CommandID: "create-thing", EventIDs: []string{"thing-created"}, Scenarios: []Scenario{},
		}},
	}
}

// TestRequiredRoleRoundTrips proves RoleList accepts both JSON shapes the
// source schema allows (a bare string, and an array) and marshals a single
// value back out as a bare string rather than always widening it.
func TestRequiredRoleRoundTrips(t *testing.T) {
	var single Command
	if err := json.Unmarshal([]byte(`{"name":"X","requiredRole":"manager"}`), &single); err != nil {
		t.Fatal(err)
	}
	if len(single.RequiredRole) != 1 || single.RequiredRole[0] != "manager" {
		t.Fatalf("expected [manager], got %v", single.RequiredRole)
	}
	out, err := json.Marshal(single.RequiredRole)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"manager"` {
		t.Errorf("a single-role list should marshal back to a bare string, got %s", out)
	}

	var multi Command
	if err := json.Unmarshal([]byte(`{"name":"X","requiredRole":["manager","administrator"]}`), &multi); err != nil {
		t.Fatal(err)
	}
	if len(multi.RequiredRole) != 2 {
		t.Fatalf("expected 2 roles, got %v", multi.RequiredRole)
	}

	var bad Command
	if err := json.Unmarshal([]byte(`{"name":"X","requiredRole":42}`), &bad); err == nil {
		t.Error("requiredRole must reject a non-string, non-array value")
	}
}

// TestFieldGatedRoleLint proves onboard-staff's real shape lints clean, and
// that a missing field/requiredRole is caught.
func TestFieldGatedRoleLint(t *testing.T) {
	doc := baseValidDoc()
	cmd := doc.Commands["create-thing"]
	cmd.FieldGatedRole = &CommandFieldGatedRole{
		Field: "role", Value: "manager", RequiredRole: RoleList{"administrator"},
	}
	doc.Commands["create-thing"] = cmd
	if err := Lint(doc).Err(); err != nil {
		t.Fatalf("a well-formed fieldGatedRole should lint clean: %v", err)
	}

	bad := baseValidDoc()
	c := bad.Commands["create-thing"]
	c.FieldGatedRole = &CommandFieldGatedRole{Value: "manager"} // missing field + requiredRole
	bad.Commands["create-thing"] = c
	err := Lint(bad).Err()
	if err == nil {
		t.Fatal("a fieldGatedRole missing field/requiredRole must be rejected")
	}
	if !strings.Contains(err.Error(), "fieldGatedRole is missing its field name") ||
		!strings.Contains(err.Error(), "fieldGatedRole is missing requiredRole") {
		t.Errorf("the error should name both gaps: %v", err)
	}
}

// TestRequiredOwnershipLint proves update-time-entry's real shape (no
// bypassRoles) lints clean, and that a dangling via.readModelId is caught.
func TestRequiredOwnershipLint(t *testing.T) {
	doc := baseValidDoc()
	cmd := doc.Commands["create-thing"]
	cmd.RequiredOwnership = &CommandOwnership{
		Via: CommandOwnershipVia{ReadModelID: "things", KeyField: "thingId", OwnerField: "ownerId"},
	}
	doc.Commands["create-thing"] = cmd
	if err := Lint(doc).Err(); err != nil {
		t.Fatalf("a well-formed requiredOwnership should lint clean: %v", err)
	}

	bad := baseValidDoc()
	c := bad.Commands["create-thing"]
	c.RequiredOwnership = &CommandOwnership{
		Via: CommandOwnershipVia{ReadModelID: "does-not-exist", KeyField: "thingId", OwnerField: "ownerId"},
	}
	bad.Commands["create-thing"] = c
	err := Lint(bad).Err()
	if err == nil || !strings.Contains(err.Error(), `references read model "does-not-exist", which does not exist`) {
		t.Fatalf("a dangling requiredOwnership.via.readModelId must be rejected: %v", err)
	}
}

// TestScopeLint proves flag-time-entry's real shape (Manager bypass, two
// read-model hops) lints clean, and that a dangling memberOfVia.readModelId
// is caught independently of resolveVia.
func TestScopeLint(t *testing.T) {
	doc := baseValidDoc()
	cmd := doc.Commands["create-thing"]
	cmd.Scope = &CommandScope{
		BypassRoles: []string{"manager"},
		ResolveVia:  CommandScopeResolveVia{ReadModelID: "things", KeyField: "thingId", SelectField: "projectId"},
		MemberOfVia: CommandScopeMemberOfVia{ReadModelID: "project-managers", MatchField: "projectId"},
	}
	doc.Commands["create-thing"] = cmd
	if err := Lint(doc).Err(); err != nil {
		t.Fatalf("a well-formed scope should lint clean: %v", err)
	}

	bad := baseValidDoc()
	c := bad.Commands["create-thing"]
	c.Scope = &CommandScope{
		ResolveVia:  CommandScopeResolveVia{ReadModelID: "things", KeyField: "thingId", SelectField: "projectId"},
		MemberOfVia: CommandScopeMemberOfVia{ReadModelID: "nope", MatchField: "projectId"},
	}
	bad.Commands["create-thing"] = c
	err := Lint(bad).Err()
	if err == nil || !strings.Contains(err.Error(), `scope.memberOfVia references read model "nope", which does not exist`) {
		t.Fatalf("a dangling scope.memberOfVia.readModelId must be rejected: %v", err)
	}
}

// TestCommandAuthNoDeclarationLintsClean proves the additive/optional
// posture: a document with none of the four keywords (every real document
// before schema 2.5.0) is unaffected.
func TestCommandAuthNoDeclarationLintsClean(t *testing.T) {
	if err := Lint(baseValidDoc()).Err(); err != nil {
		t.Fatalf("a document declaring none of the four keywords must still lint clean: %v", err)
	}
}
