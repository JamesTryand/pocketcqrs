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
					{Name: "regionId", Type: "string"},
				},
			},
			"region-members": {
				Name:              "Region Members",
				BuiltFromEventIDs: []string{"thing-created"},
				Fields: []Field{
					{Name: "assignmentId", Type: "string", IDAttribute: true},
					{Name: "regionId", Type: "string"},
					{Name: "memberId", Type: "string"},
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
	if err := json.Unmarshal([]byte(`{"name":"X","requiredRole":"supervisor"}`), &single); err != nil {
		t.Fatal(err)
	}
	if len(single.RequiredRole) != 1 || single.RequiredRole[0] != "supervisor" {
		t.Fatalf("expected [supervisor], got %v", single.RequiredRole)
	}
	out, err := json.Marshal(single.RequiredRole)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"supervisor"` {
		t.Errorf("a single-role list should marshal back to a bare string, got %s", out)
	}

	var multi Command
	if err := json.Unmarshal([]byte(`{"name":"X","requiredRole":["supervisor","admin"]}`), &multi); err != nil {
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

// TestFieldGatedRoleLint proves a field-gated invitation lints clean, and
// that a missing field/requiredRole is caught.
func TestFieldGatedRoleLint(t *testing.T) {
	doc := baseValidDoc()
	cmd := doc.Commands["create-thing"]
	cmd.FieldGatedRole = &CommandFieldGatedRole{
		Field: "role", Value: "supervisor", RequiredRole: RoleList{"admin"},
	}
	doc.Commands["create-thing"] = cmd
	if err := Lint(doc).Err(); err != nil {
		t.Fatalf("a well-formed fieldGatedRole should lint clean: %v", err)
	}

	bad := baseValidDoc()
	c := bad.Commands["create-thing"]
	c.FieldGatedRole = &CommandFieldGatedRole{Value: "supervisor"} // missing field + requiredRole
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

// TestRequiredOwnershipLint proves an owner-only update's shape (no
// bypassRoles) lints clean, and that a dangling via.readModelId is caught.
func TestRequiredOwnershipLint(t *testing.T) {
	doc := baseValidDoc()
	cmd := doc.Commands["create-thing"]
	cmd.RequiredOwnership = &CommandOwnership{
		Via: &CommandOwnershipVia{ReadModelID: "things", KeyField: "thingId", OwnerField: "ownerId"},
	}
	doc.Commands["create-thing"] = cmd
	if err := Lint(doc).Err(); err != nil {
		t.Fatalf("a well-formed requiredOwnership should lint clean: %v", err)
	}

	bad := baseValidDoc()
	c := bad.Commands["create-thing"]
	c.RequiredOwnership = &CommandOwnership{
		Via: &CommandOwnershipVia{ReadModelID: "does-not-exist", KeyField: "thingId", OwnerField: "ownerId"},
	}
	bad.Commands["create-thing"] = c
	err := Lint(bad).Err()
	if err == nil || !strings.Contains(err.Error(), `references read model "does-not-exist", which does not exist`) {
		t.Fatalf("a dangling requiredOwnership.via.readModelId must be rejected: %v", err)
	}
}

// TestScopeLint proves a scope (a bypass role, two read-model hops) lints
// clean, that a dangling memberOfVia.readModelId is caught independently of
// resolveVia, and that a scope must name the member's field (4.0.0).
func TestScopeLint(t *testing.T) {
	doc := baseValidDoc()
	cmd := doc.Commands["create-thing"]
	cmd.Scope = &CommandScope{
		BypassRoles: []string{"supervisor"},
		ResolveVia:  CommandScopeResolveVia{ReadModelID: "things", KeyField: "thingId", SelectField: "regionId"},
		MemberOfVia: CommandScopeMemberOfVia{ReadModelID: "region-members", MatchField: "regionId", ActorField: "memberId"},
	}
	doc.Commands["create-thing"] = cmd
	if err := Lint(doc).Err(); err != nil {
		t.Fatalf("a well-formed scope should lint clean: %v", err)
	}

	bad := baseValidDoc()
	c := bad.Commands["create-thing"]
	c.Scope = &CommandScope{
		ResolveVia:  CommandScopeResolveVia{ReadModelID: "things", KeyField: "thingId", SelectField: "regionId"},
		MemberOfVia: CommandScopeMemberOfVia{ReadModelID: "nope", MatchField: "regionId", ActorField: "memberId"},
	}
	bad.Commands["create-thing"] = c
	err := Lint(bad).Err()
	if err == nil || !strings.Contains(err.Error(), `scope.memberOfVia references read model "nope", which does not exist`) {
		t.Fatalf("a dangling scope.memberOfVia.readModelId must be rejected: %v", err)
	}

	unnamed := baseValidDoc()
	u := unnamed.Commands["create-thing"]
	u.Scope = &CommandScope{
		ResolveVia:  CommandScopeResolveVia{ReadModelID: "things", KeyField: "thingId", SelectField: "regionId"},
		MemberOfVia: CommandScopeMemberOfVia{ReadModelID: "region-members", MatchField: "regionId"},
	}
	unnamed.Commands["create-thing"] = u
	err = Lint(unnamed).Err()
	if err == nil || !strings.Contains(err.Error(), "actorField") {
		t.Fatalf("a scope that doesn't name the member's field must be rejected, naming actorField: %v", err)
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

// requiredOwnership is exactly one of via or stateField (schema 3.15.0), and
// bypassRoles goes with via only: a decision knows its actor, not their role.
func TestOwnershipFormsAreLinted(t *testing.T) {
	via := &CommandOwnershipVia{ReadModelID: "things", KeyField: "thingId", OwnerField: "ownerId"}
	for name, c := range map[string]struct {
		own *CommandOwnership
		ok  bool
	}{
		"via":                 {&CommandOwnership{Via: via}, true},
		"stateField":          {&CommandOwnership{StateField: "ownerId"}, true},
		"both":                {&CommandOwnership{Via: via, StateField: "ownerId"}, false},
		"neither":             {&CommandOwnership{}, false},
		"stateField + bypass": {&CommandOwnership{StateField: "ownerId", BypassRoles: []string{"supervisor"}}, false},
	} {
		doc := &Document{
			EventModelingSchemaVersion: SchemaVersion,
			Commands:                   map[string]Command{"c": {Name: "C", Aggregate: "thing", RequiredOwnership: c.own}},
			ReadModels:                 map[string]ReadModel{"things": {Name: "Things"}},
		}
		var ownErrs []string
		for _, e := range Lint(doc).Errors {
			if strings.Contains(e, "requiredOwnership") {
				ownErrs = append(ownErrs, e)
			}
		}
		if (len(ownErrs) == 0) != c.ok {
			t.Errorf("%s: ok=%v, errors %v", name, c.ok, ownErrs)
		}
	}
}
