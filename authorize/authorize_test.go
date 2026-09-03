package authorize

import (
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/emschema"
)

// testDoc mirrors project/timesheets' own real shapes closely enough to be
// a meaningful fixture: a Manager-only command, an onboard-staff-shaped
// fieldGatedRole, an ownership-only command (no bypass, the real
// update-time-entry shape), and a scope command (Manager bypass, the real
// flag-time-entry shape) — plus one command with no declaration at all, to
// prove BuildPolicies leaves it out of the table entirely.
func testDoc() *emschema.Document {
	return &emschema.Document{
		Commands: map[string]emschema.Command{
			"set-rate-override": {Name: "Set Rate Override", Aggregate: "rate_override",
				RequiredRole: emschema.RoleList{"manager"}},
			"onboard-staff": {Name: "Onboard Staff", Aggregate: "staff",
				FieldGatedRole: &emschema.CommandFieldGatedRole{
					Field: "role", Value: "manager", RequiredRole: emschema.RoleList{"administrator"},
				}},
			"update-time-entry": {Name: "Update Time Entry", Aggregate: "time_entry",
				RequiredOwnership: &emschema.CommandOwnership{
					Via: emschema.CommandOwnershipVia{ReadModelID: "time-entries", KeyField: "entryId", OwnerField: "staffId"},
				}},
			"flag-time-entry": {Name: "Flag Time Entry", Aggregate: "time_entry",
				Scope: &emschema.CommandScope{
					BypassRoles: []string{"manager"},
					ResolveVia:  emschema.CommandScopeResolveVia{ReadModelID: "time-entries", KeyField: "entryId", SelectField: "projectId"},
					MemberOfVia: emschema.CommandScopeMemberOfVia{ReadModelID: "project-managers", MatchField: "projectId"},
				}},
			"log-time-entry": {Name: "Log Time Entry", Aggregate: "time_entry"}, // no declaration at all
		},
		ReadModels: map[string]emschema.ReadModel{
			"time-entries":     {Name: "Time Entries"},
			"project-managers": {Name: "Project Managers"},
		},
	}
}

func TestBuildPolicies(t *testing.T) {
	policies, err := BuildPolicies(testDoc())
	if err != nil {
		t.Fatal(err)
	}

	if len(policies) != 4 {
		t.Fatalf("expected 4 policies (the undeclared command excluded), got %d: %v", len(policies), policies)
	}

	rate := policies[Key{"rate_override", "SetRateOverride"}]
	if len(rate.RequiredRole) != 1 || rate.RequiredRole[0] != "manager" {
		t.Errorf("SetRateOverride: expected requiredRole [manager], got %+v", rate)
	}

	onboard := policies[Key{"staff", "OnboardStaff"}]
	if onboard.FieldGatedRole == nil || onboard.FieldGatedRole.Field != "role" ||
		onboard.FieldGatedRole.Value != "manager" || len(onboard.FieldGatedRole.RequiredRole) != 1 ||
		onboard.FieldGatedRole.RequiredRole[0] != "administrator" {
		t.Errorf("OnboardStaff: unexpected fieldGatedRole: %+v", onboard.FieldGatedRole)
	}

	update := policies[Key{"time_entry", "UpdateTimeEntry"}]
	if update.Ownership == nil || update.Ownership.Collection != "timeEntries" ||
		update.Ownership.KeyField != "entryId" || update.Ownership.OwnerField != "staffId" ||
		len(update.Ownership.BypassRoles) != 0 {
		t.Errorf("UpdateTimeEntry: unexpected ownership: %+v", update.Ownership)
	}

	flag := policies[Key{"time_entry", "FlagTimeEntry"}]
	if flag.Scope == nil || flag.Scope.ResolveCollection != "timeEntries" || flag.Scope.ResolveKeyField != "entryId" ||
		flag.Scope.ResolveSelectField != "projectId" || flag.Scope.MemberOfCollection != "projectManagers" ||
		flag.Scope.MemberOfMatchField != "projectId" || len(flag.Scope.BypassRoles) != 1 || flag.Scope.BypassRoles[0] != "manager" {
		t.Errorf("FlagTimeEntry: unexpected scope: %+v", flag.Scope)
	}

	if _, ok := policies[Key{"time_entry", "LogTimeEntry"}]; ok {
		t.Error("LogTimeEntry declares nothing and must not appear in the table")
	}
}

func TestBuildPoliciesRefusesAMissingAggregateTag(t *testing.T) {
	doc := &emschema.Document{Commands: map[string]emschema.Command{
		"do-thing": {Name: "Do Thing", RequiredRole: emschema.RoleList{"manager"}}, // no Aggregate
	}}
	if _, err := BuildPolicies(doc); err == nil {
		t.Fatal("a command declaring an auth requirement with no aggregate tag must be refused")
	}
}

func TestBuildPoliciesRefusesADanglingReadModelReference(t *testing.T) {
	doc := &emschema.Document{Commands: map[string]emschema.Command{
		"update-thing": {Name: "Update Thing", Aggregate: "thing", RequiredOwnership: &emschema.CommandOwnership{
			Via: emschema.CommandOwnershipVia{ReadModelID: "does-not-exist", KeyField: "id", OwnerField: "ownerId"},
		}},
	}}
	if _, err := BuildPolicies(doc); err == nil {
		t.Fatal("a dangling requiredOwnership.via.readModelId must be refused (defensively — Lint should already catch this)")
	}
}

// ---------------------------------------------------------------------
// Authorizer.Authorize — real PocketBase collections/records, same
// tests.NewTestApp() harness gateway_test.go uses, so ownership/scope
// evaluation exercises the real FindFirstRecordByFilter path rather than a
// mock.
// ---------------------------------------------------------------------

func newTestApp(t *testing.T) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	return app
}

func mustCreateCollection(t *testing.T, app core.App, name string, fields ...string) *core.Collection {
	t.Helper()
	col := core.NewBaseCollection(name)
	for _, f := range fields {
		col.Fields.Add(&core.TextField{Name: f})
	}
	if err := app.Save(col); err != nil {
		t.Fatalf("creating collection %s: %v", name, err)
	}
	return col
}

func mustInsertRow(t *testing.T, app core.App, col *core.Collection, fields map[string]string) {
	t.Helper()
	rec := core.NewRecord(col)
	for k, v := range fields {
		rec.Set(k, v)
	}
	if err := app.Save(rec); err != nil {
		t.Fatalf("inserting into %s: %v", col.Name, err)
	}
}

func TestAuthorize_NoPolicy_Allows(t *testing.T) {
	a := &Authorizer{Policies: map[Key]Policy{}}
	ok, err := a.Authorize(newTestApp(t), "thing", "DoSomething", "t1", nil, nil)
	if err != nil || !ok {
		t.Fatalf("a command with no declared policy must be allowed, got ok=%v err=%v", ok, err)
	}
}

func TestAuthorize_RequiredRole(t *testing.T) {
	a := &Authorizer{
		Policies:       map[Key]Policy{{"rate_override", "SetRateOverride"}: {RequiredRole: []string{"manager"}}},
		ResolveOwnRole: func(*core.Record) string { return "manager" },
	}
	ok, err := a.Authorize(newTestApp(t), "rate_override", "SetRateOverride", "r1", nil, nil)
	if err != nil || !ok {
		t.Fatalf("manager should be authorized, got ok=%v err=%v", ok, err)
	}

	a.ResolveOwnRole = func(*core.Record) string { return "staff" }
	ok, err = a.Authorize(newTestApp(t), "rate_override", "SetRateOverride", "r1", nil, nil)
	if err != nil || ok {
		t.Fatalf("staff should be refused, got ok=%v err=%v", ok, err)
	}

	a.ResolveOwnRole = func(*core.Record) string { return "MANAGER" }
	ok, err = a.Authorize(newTestApp(t), "rate_override", "SetRateOverride", "r1", nil, nil)
	if err != nil || !ok {
		t.Fatalf("the role match must be case-insensitive, got ok=%v err=%v", ok, err)
	}
}

func TestAuthorize_FieldGatedRole(t *testing.T) {
	a := &Authorizer{
		Policies: map[Key]Policy{{"staff", "OnboardStaff"}: {FieldGatedRole: &FieldGatedRolePolicy{
			Field: "role", Value: "manager", RequiredRole: []string{"administrator"},
		}}},
	}
	app := newTestApp(t)

	// role=manager in the payload, actor is administrator -> allowed
	a.ResolveOwnRole = func(*core.Record) string { return "administrator" }
	ok, err := a.Authorize(app, "staff", "OnboardStaff", "s1", []byte(`{"role":"manager"}`), nil)
	if err != nil || !ok {
		t.Fatalf("an administrator onboarding a manager should be allowed, got ok=%v err=%v", ok, err)
	}

	// role=manager in the payload, actor is a plain manager (not administrator) -> refused
	a.ResolveOwnRole = func(*core.Record) string { return "manager" }
	ok, err = a.Authorize(app, "staff", "OnboardStaff", "s1", []byte(`{"role":"manager"}`), nil)
	if err != nil || ok {
		t.Fatalf("a Manager (not Administrator) onboarding a Manager should be refused, got ok=%v err=%v", ok, err)
	}

	// role=staff in the payload -> condition doesn't apply, anyone may
	a.ResolveOwnRole = func(*core.Record) string { return "" }
	ok, err = a.Authorize(app, "staff", "OnboardStaff", "s1", []byte(`{"role":"staff"}`), nil)
	if err != nil || !ok {
		t.Fatalf("onboarding a plain staff member should need nobody in particular, got ok=%v err=%v", ok, err)
	}
}

func TestAuthorize_Ownership(t *testing.T) {
	app := newTestApp(t)
	col := mustCreateCollection(t, app, "timeEntries", "entryId", "staffId")
	mustInsertRow(t, app, col, map[string]string{"entryId": "e1", "staffId": "staff1"})

	a := &Authorizer{
		Policies: map[Key]Policy{{"time_entry", "UpdateTimeEntry"}: {Ownership: &OwnershipPolicy{
			Collection: "timeEntries", KeyField: "entryId", OwnerField: "staffId",
		}}},
		ResolveOwnRole:    func(*core.Record) string { return "staff" },
		ResolveOwnStaffID: func(*core.Record) string { return "staff1" },
	}

	// the owner may update their own entry
	ok, err := a.Authorize(app, "time_entry", "UpdateTimeEntry", "e1", nil, nil)
	if err != nil || !ok {
		t.Fatalf("the owning staff member should be authorized, got ok=%v err=%v", ok, err)
	}

	// a different staff member may not
	a.ResolveOwnStaffID = func(*core.Record) string { return "staff2" }
	ok, err = a.Authorize(app, "time_entry", "UpdateTimeEntry", "e1", nil, nil)
	if err != nil || ok {
		t.Fatalf("a non-owning staff member should be refused, got ok=%v err=%v", ok, err)
	}

	// a nonexistent entry is refused, not an error
	ok, err = a.Authorize(app, "time_entry", "UpdateTimeEntry", "does-not-exist", nil, nil)
	if err != nil || ok {
		t.Fatalf("a nonexistent target should be refused cleanly, got ok=%v err=%v", ok, err)
	}
}

func TestAuthorize_OwnershipBypass(t *testing.T) {
	app := newTestApp(t)
	col := mustCreateCollection(t, app, "timeEntries", "entryId", "staffId")
	mustInsertRow(t, app, col, map[string]string{"entryId": "e1", "staffId": "staff1"})

	a := &Authorizer{
		Policies: map[Key]Policy{{"time_entry", "UpdateTimeEntry"}: {Ownership: &OwnershipPolicy{
			Collection: "timeEntries", KeyField: "entryId", OwnerField: "staffId", BypassRoles: []string{"manager"},
		}}},
		ResolveOwnRole:    func(*core.Record) string { return "manager" },
		ResolveOwnStaffID: func(*core.Record) string { return "someone-else-entirely" },
	}
	ok, err := a.Authorize(app, "time_entry", "UpdateTimeEntry", "e1", nil, nil)
	if err != nil || !ok {
		t.Fatalf("a bypass role should short-circuit the ownership check entirely, got ok=%v err=%v", ok, err)
	}
}

func TestAuthorize_Scope(t *testing.T) {
	app := newTestApp(t)
	entries := mustCreateCollection(t, app, "timeEntries", "entryId", "projectId")
	mustInsertRow(t, app, entries, map[string]string{"entryId": "e1", "projectId": "p1"})
	pms := mustCreateCollection(t, app, "projectManagers", "projectId", "staffId")
	mustInsertRow(t, app, pms, map[string]string{"projectId": "p1", "staffId": "pm1"})

	a := &Authorizer{
		Policies: map[Key]Policy{{"time_entry", "FlagTimeEntry"}: {Scope: &ScopePolicy{
			ResolveCollection: "timeEntries", ResolveKeyField: "entryId", ResolveSelectField: "projectId",
			MemberOfCollection: "projectManagers", MemberOfMatchField: "projectId",
			BypassRoles: []string{"manager"},
		}}},
	}

	// Manager bypasses unconditionally
	a.ResolveOwnRole = func(*core.Record) string { return "manager" }
	a.ResolveOwnStaffID = func(*core.Record) string { return "irrelevant" }
	ok, err := a.Authorize(app, "time_entry", "FlagTimeEntry", "e1", nil, nil)
	if err != nil || !ok {
		t.Fatalf("Manager should bypass the scope check, got ok=%v err=%v", ok, err)
	}

	// the entry's own project's PM may flag it
	a.ResolveOwnRole = func(*core.Record) string { return "staff" }
	a.ResolveOwnStaffID = func(*core.Record) string { return "pm1" }
	ok, err = a.Authorize(app, "time_entry", "FlagTimeEntry", "e1", nil, nil)
	if err != nil || !ok {
		t.Fatalf("the entry's own project's PM should be authorized, got ok=%v err=%v", ok, err)
	}

	// a PM of a DIFFERENT project may not
	a.ResolveOwnStaffID = func(*core.Record) string { return "pm2" }
	ok, err = a.Authorize(app, "time_entry", "FlagTimeEntry", "e1", nil, nil)
	if err != nil || ok {
		t.Fatalf("a PM of a different project should be refused, got ok=%v err=%v", ok, err)
	}

	// a plain staff member (not a PM of anything) may not
	a.ResolveOwnStaffID = func(*core.Record) string { return "staff1" }
	ok, err = a.Authorize(app, "time_entry", "FlagTimeEntry", "e1", nil, nil)
	if err != nil || ok {
		t.Fatalf("a plain staff member should be refused, got ok=%v err=%v", ok, err)
	}

	// a nonexistent entry is refused, not an error (resolve hop finds nothing)
	a.ResolveOwnRole = func(*core.Record) string { return "staff" }
	ok, err = a.Authorize(app, "time_entry", "FlagTimeEntry", "does-not-exist", nil, nil)
	if err != nil || ok {
		t.Fatalf("a nonexistent target should be refused cleanly, got ok=%v err=%v", ok, err)
	}
}

func TestAuthorize_NilResolversTreatedAsNoActor(t *testing.T) {
	// Neither resolver set at all -- an Authorizer wired with no
	// project-specific delegates must not panic, and must refuse a
	// role/ownership-gated command (there's no actor to satisfy it with).
	app := newTestApp(t)
	a := &Authorizer{Policies: map[Key]Policy{{"rate_override", "SetRateOverride"}: {RequiredRole: []string{"manager"}}}}
	ok, err := a.Authorize(app, "rate_override", "SetRateOverride", "r1", nil, nil)
	if err != nil || ok {
		t.Fatalf("no resolvers configured should mean no role satisfies a requirement, got ok=%v err=%v", ok, err)
	}
}
