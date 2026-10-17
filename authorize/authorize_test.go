package authorize

import (
	"errors"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/cmdauth"
	"github.com/jamestryand/pocketcqrs/emschema"
)

// testDoc is one command of each kind: a supervisor-only command, a
// field-gated invitation, an ownership-only command (no bypass), and a
// scope command (supervisor bypass) — plus one command with no declaration
// at all, to prove BuildPolicies leaves it out of the table entirely.
func testDoc() *emschema.Document {
	return &emschema.Document{
		Commands: map[string]emschema.Command{
			"set-price": {Name: "Set Price", Aggregate: "price",
				RequiredRole: emschema.RoleList{"supervisor"}},
			"invite-user": {Name: "Invite User", Aggregate: "user",
				FieldGatedRole: &emschema.CommandFieldGatedRole{
					Field: "role", Value: "supervisor", RequiredRole: emschema.RoleList{"admin"},
				}},
			"update-order": {Name: "Update Order", Aggregate: "order",
				RequiredOwnership: &emschema.CommandOwnership{
					Via: &emschema.CommandOwnershipVia{ReadModelID: "orders", KeyField: "orderId", OwnerField: "ownerId"},
				}},
			"flag-order": {Name: "Flag Order", Aggregate: "order",
				Scope: &emschema.CommandScope{
					BypassRoles: []string{"supervisor"},
					ResolveVia:  emschema.CommandScopeResolveVia{ReadModelID: "orders", KeyField: "orderId", SelectField: "regionId"},
					MemberOfVia: emschema.CommandScopeMemberOfVia{ReadModelID: "region-members", MatchField: "regionId", ActorField: "memberId"},
					Message:     "only a member of the order's region may flag it",
				}},
			"place-order": {Name: "Place Order", Aggregate: "order"}, // no declaration at all
		},
		ReadModels: map[string]emschema.ReadModel{
			"orders":         {Name: "Orders"},
			"region-members": {Name: "Region Members"},
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

	price := policies[Key{"price", "SetPrice"}]
	if len(price.RequiredRole) != 1 || price.RequiredRole[0] != "supervisor" {
		t.Errorf("SetPrice: expected requiredRole [supervisor], got %+v", price)
	}

	invite := policies[Key{"user", "InviteUser"}]
	if invite.FieldGatedRole == nil || invite.FieldGatedRole.Field != "role" ||
		invite.FieldGatedRole.Value != "supervisor" || len(invite.FieldGatedRole.RequiredRole) != 1 ||
		invite.FieldGatedRole.RequiredRole[0] != "admin" {
		t.Errorf("InviteUser: unexpected fieldGatedRole: %+v", invite.FieldGatedRole)
	}

	update := policies[Key{"order", "UpdateOrder"}]
	if update.Ownership == nil || update.Ownership.Collection != "orders" ||
		update.Ownership.KeyField != "orderId" || update.Ownership.OwnerField != "ownerId" ||
		len(update.Ownership.BypassRoles) != 0 {
		t.Errorf("UpdateOrder: unexpected ownership: %+v", update.Ownership)
	}

	flag := policies[Key{"order", "FlagOrder"}]
	if flag.Scope == nil || flag.Scope.ResolveCollection != "orders" || flag.Scope.ResolveKeyField != "orderId" ||
		flag.Scope.ResolveSelectField != "regionId" || flag.Scope.MemberOfCollection != "regionMembers" ||
		flag.Scope.MemberOfMatchField != "regionId" || flag.Scope.MemberOfActorField != "memberId" ||
		len(flag.Scope.BypassRoles) != 1 || flag.Scope.BypassRoles[0] != "supervisor" ||
		flag.Scope.Message != "only a member of the order's region may flag it" {
		t.Errorf("FlagOrder: unexpected scope: %+v", flag.Scope)
	}

	if _, ok := policies[Key{"order", "PlaceOrder"}]; ok {
		t.Error("PlaceOrder declares nothing and must not appear in the table")
	}
}

func TestBuildPoliciesRefusesAMissingAggregateTag(t *testing.T) {
	doc := &emschema.Document{Commands: map[string]emschema.Command{
		"do-thing": {Name: "Do Thing", RequiredRole: emschema.RoleList{"supervisor"}}, // no Aggregate
	}}
	if _, err := BuildPolicies(doc); err == nil {
		t.Fatal("a command declaring an auth requirement with no aggregate tag must be refused")
	}
}

func TestBuildPoliciesRefusesADanglingReadModelReference(t *testing.T) {
	doc := &emschema.Document{Commands: map[string]emschema.Command{
		"update-thing": {Name: "Update Thing", Aggregate: "thing", RequiredOwnership: &emschema.CommandOwnership{
			Via: &emschema.CommandOwnershipVia{ReadModelID: "does-not-exist", KeyField: "id", OwnerField: "ownerId"},
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
		Policies:       map[Key]Policy{{"price", "SetPrice"}: {RequiredRole: []string{"supervisor"}}},
		ResolveOwnRole: func(*core.Record) string { return "supervisor" },
	}
	ok, err := a.Authorize(newTestApp(t), "price", "SetPrice", "r1", nil, nil)
	if err != nil || !ok {
		t.Fatalf("a supervisor should be authorized, got ok=%v err=%v", ok, err)
	}

	a.ResolveOwnRole = func(*core.Record) string { return "member" }
	ok, err = a.Authorize(newTestApp(t), "price", "SetPrice", "r1", nil, nil)
	if err != nil || ok {
		t.Fatalf("a member should be refused, got ok=%v err=%v", ok, err)
	}

	a.ResolveOwnRole = func(*core.Record) string { return "SUPERVISOR" }
	ok, err = a.Authorize(newTestApp(t), "price", "SetPrice", "r1", nil, nil)
	if err != nil || !ok {
		t.Fatalf("the role match must be case-insensitive, got ok=%v err=%v", ok, err)
	}
}

func TestAuthorize_FieldGatedRole(t *testing.T) {
	a := &Authorizer{
		Policies: map[Key]Policy{{"user", "InviteUser"}: {FieldGatedRole: &FieldGatedRolePolicy{
			Field: "role", Value: "supervisor", RequiredRole: []string{"admin"},
		}}},
	}
	app := newTestApp(t)

	// role=supervisor in the payload, actor is an admin -> allowed
	a.ResolveOwnRole = func(*core.Record) string { return "admin" }
	ok, err := a.Authorize(app, "user", "InviteUser", "u1", []byte(`{"role":"supervisor"}`), nil)
	if err != nil || !ok {
		t.Fatalf("an admin inviting a supervisor should be allowed, got ok=%v err=%v", ok, err)
	}

	// role=supervisor in the payload, actor is a supervisor (not an admin) -> refused
	a.ResolveOwnRole = func(*core.Record) string { return "supervisor" }
	ok, err = a.Authorize(app, "user", "InviteUser", "u1", []byte(`{"role":"supervisor"}`), nil)
	if err != nil || ok {
		t.Fatalf("a supervisor (not an admin) inviting a supervisor should be refused, got ok=%v err=%v", ok, err)
	}

	// role=member in the payload -> condition doesn't apply, anyone may
	a.ResolveOwnRole = func(*core.Record) string { return "" }
	ok, err = a.Authorize(app, "user", "InviteUser", "u1", []byte(`{"role":"member"}`), nil)
	if err != nil || !ok {
		t.Fatalf("inviting a plain member should need nobody in particular, got ok=%v err=%v", ok, err)
	}
}

func TestAuthorize_Ownership(t *testing.T) {
	app := newTestApp(t)
	col := mustCreateCollection(t, app, "orders", "orderId", "ownerId")
	mustInsertRow(t, app, col, map[string]string{"orderId": "o1", "ownerId": "u1"})

	a := &Authorizer{
		Policies: map[Key]Policy{{"order", "UpdateOrder"}: {Ownership: &OwnershipPolicy{
			Collection: "orders", KeyField: "orderId", OwnerField: "ownerId",
		}}},
		ResolveOwnRole:      func(*core.Record) string { return "member" },
		ResolveOwnSubjectID: func(*core.Record) string { return "u1" },
	}

	// the owner may update their own order
	ok, err := a.Authorize(app, "order", "UpdateOrder", "o1", nil, nil)
	if err != nil || !ok {
		t.Fatalf("the owner should be authorized, got ok=%v err=%v", ok, err)
	}

	// someone else may not
	a.ResolveOwnSubjectID = func(*core.Record) string { return "u2" }
	ok, err = a.Authorize(app, "order", "UpdateOrder", "o1", nil, nil)
	if err != nil || ok {
		t.Fatalf("someone other than the owner should be refused, got ok=%v err=%v", ok, err)
	}

	// a nonexistent order is refused, not an error
	ok, err = a.Authorize(app, "order", "UpdateOrder", "does-not-exist", nil, nil)
	if err != nil || ok {
		t.Fatalf("a nonexistent target should be refused cleanly, got ok=%v err=%v", ok, err)
	}
}

func TestAuthorize_OwnershipBypass(t *testing.T) {
	app := newTestApp(t)
	col := mustCreateCollection(t, app, "orders", "orderId", "ownerId")
	mustInsertRow(t, app, col, map[string]string{"orderId": "o1", "ownerId": "u1"})

	a := &Authorizer{
		Policies: map[Key]Policy{{"order", "UpdateOrder"}: {Ownership: &OwnershipPolicy{
			Collection: "orders", KeyField: "orderId", OwnerField: "ownerId", BypassRoles: []string{"supervisor"},
		}}},
		ResolveOwnRole:      func(*core.Record) string { return "supervisor" },
		ResolveOwnSubjectID: func(*core.Record) string { return "someone-else-entirely" },
	}
	ok, err := a.Authorize(app, "order", "UpdateOrder", "o1", nil, nil)
	if err != nil || !ok {
		t.Fatalf("a bypass role should short-circuit the ownership check entirely, got ok=%v err=%v", ok, err)
	}
}

func TestAuthorize_Scope(t *testing.T) {
	app := newTestApp(t)
	orders := mustCreateCollection(t, app, "orders", "orderId", "regionId")
	mustInsertRow(t, app, orders, map[string]string{"orderId": "o1", "regionId": "north"})
	// addedBy holds an actor id too, so a check comparing the wrong field
	// would let the person who added a member act as one
	members := mustCreateCollection(t, app, "regionMembers", "regionId", "memberId", "addedBy")
	mustInsertRow(t, app, members, map[string]string{"regionId": "north", "memberId": "m1", "addedBy": "admin1"})

	a := &Authorizer{
		Policies: map[Key]Policy{{"order", "FlagOrder"}: {Scope: &ScopePolicy{
			ResolveCollection: "orders", ResolveKeyField: "orderId", ResolveSelectField: "regionId",
			MemberOfCollection: "regionMembers", MemberOfMatchField: "regionId", MemberOfActorField: "memberId",
			BypassRoles: []string{"supervisor"},
		}}},
	}

	// a supervisor bypasses unconditionally
	a.ResolveOwnRole = func(*core.Record) string { return "supervisor" }
	a.ResolveOwnSubjectID = func(*core.Record) string { return "irrelevant" }
	ok, err := a.Authorize(app, "order", "FlagOrder", "o1", nil, nil)
	if err != nil || !ok {
		t.Fatalf("a supervisor should bypass the scope check, got ok=%v err=%v", ok, err)
	}

	// a member of the order's own region may flag it
	a.ResolveOwnRole = func(*core.Record) string { return "member" }
	a.ResolveOwnSubjectID = func(*core.Record) string { return "m1" }
	ok, err = a.Authorize(app, "order", "FlagOrder", "o1", nil, nil)
	if err != nil || !ok {
		t.Fatalf("a member of the order's region should be authorized, got ok=%v err=%v", ok, err)
	}

	// the member is compared through the declared actorField, nothing else
	a.ResolveOwnSubjectID = func(*core.Record) string { return "admin1" }
	ok, err = a.Authorize(app, "order", "FlagOrder", "o1", nil, nil)
	if err != nil || ok {
		t.Fatalf("an actor found only in another field must be refused, got ok=%v err=%v", ok, err)
	}

	// someone who is no member of that region may not
	a.ResolveOwnSubjectID = func(*core.Record) string { return "m2" }
	ok, err = a.Authorize(app, "order", "FlagOrder", "o1", nil, nil)
	if err != nil || ok {
		t.Fatalf("a non-member should be refused, got ok=%v err=%v", ok, err)
	}

	// a nonexistent order is refused, not an error (resolve hop finds nothing)
	ok, err = a.Authorize(app, "order", "FlagOrder", "does-not-exist", nil, nil)
	if err != nil || ok {
		t.Fatalf("a nonexistent target should be refused cleanly, got ok=%v err=%v", ok, err)
	}
}

// A scope declaring its message (schema 4.0.0) refuses with it, as a
// *cmdauth.Refusal the gateway answers 403 with; a member is still allowed.
func TestAuthorize_ScopeRefusesWithItsMessage(t *testing.T) {
	app := newTestApp(t)
	orders := mustCreateCollection(t, app, "orders", "orderId", "regionId")
	mustInsertRow(t, app, orders, map[string]string{"orderId": "o1", "regionId": "north"})
	members := mustCreateCollection(t, app, "regionMembers", "regionId", "memberId")
	mustInsertRow(t, app, members, map[string]string{"regionId": "north", "memberId": "m1"})

	const message = "only a member of the order's region may flag it"
	a := &Authorizer{
		Policies: map[Key]Policy{{"order", "FlagOrder"}: {Scope: &ScopePolicy{
			ResolveCollection: "orders", ResolveKeyField: "orderId", ResolveSelectField: "regionId",
			MemberOfCollection: "regionMembers", MemberOfMatchField: "regionId", MemberOfActorField: "memberId",
			Message: message,
		}}},
		ResolveOwnSubjectID: func(*core.Record) string { return "m2" },
	}
	ok, err := a.Authorize(app, "order", "FlagOrder", "o1", nil, nil)
	var refusal *cmdauth.Refusal
	if ok || !errors.As(err, &refusal) || refusal.Message != message {
		t.Fatalf("a non-member should be refused with the scope's message, got ok=%v err=%v", ok, err)
	}

	a.ResolveOwnSubjectID = func(*core.Record) string { return "m1" }
	if ok, err := a.Authorize(app, "order", "FlagOrder", "o1", nil, nil); err != nil || !ok {
		t.Fatalf("a member of the order's region should be authorized, got ok=%v err=%v", ok, err)
	}
}

func TestAuthorize_NilResolversTreatedAsNoActor(t *testing.T) {
	// Neither resolver set at all -- an Authorizer wired with no
	// host-specific delegates must not panic, and must refuse a
	// role/ownership-gated command (there's no actor to satisfy it with).
	app := newTestApp(t)
	a := &Authorizer{Policies: map[Key]Policy{{"price", "SetPrice"}: {RequiredRole: []string{"supervisor"}}}}
	ok, err := a.Authorize(app, "price", "SetPrice", "r1", nil, nil)
	if err != nil || ok {
		t.Fatalf("no resolvers configured should mean no role satisfies a requirement, got ok=%v err=%v", ok, err)
	}
}

// A command's declarations combine, every one must hold (the schema's
// docs, 2026-10-14; dotnetcqrs 6649745). Evaluation used to return on the
// first declaration it met, so a role that satisfied requiredRole skipped
// the ownership check entirely, and a field gate that didn't apply allowed
// the command before scope was looked at.
func TestAuthorize_EveryDeclarationMustHold(t *testing.T) {
	app := newTestApp(t)
	orders := mustCreateCollection(t, app, "orders", "orderId", "ownerId", "regionId")
	mustInsertRow(t, app, orders, map[string]string{"orderId": "o1", "ownerId": "u1", "regionId": "north"})
	members := mustCreateCollection(t, app, "regionMembers", "regionId", "memberId")
	mustInsertRow(t, app, members, map[string]string{"regionId": "north", "memberId": "m1"})

	ownership := &OwnershipPolicy{Collection: "orders", KeyField: "orderId", OwnerField: "ownerId"}
	scope := &ScopePolicy{
		ResolveCollection: "orders", ResolveKeyField: "orderId", ResolveSelectField: "regionId",
		MemberOfCollection: "regionMembers", MemberOfMatchField: "regionId", MemberOfActorField: "memberId",
	}
	gate := &FieldGatedRolePolicy{Field: "role", Value: "supervisor", RequiredRole: []string{"admin"}}

	cases := []struct {
		name      string
		policy    Policy
		role      string
		subjectID string
		payload   string
		want      bool
	}{
		// requiredRole + ownership: member holds the role, but o1 is u1's
		{"role held, not the owner", Policy{RequiredRole: []string{"member"}, Ownership: ownership}, "member", "u2", `{}`, false},
		{"role held, the owner", Policy{RequiredRole: []string{"member"}, Ownership: ownership}, "member", "u1", `{}`, true},
		{"the owner, role not held", Policy{RequiredRole: []string{"supervisor"}, Ownership: ownership}, "member", "u1", `{}`, false},
		// requiredRole + an applying field gate: a supervisor holds
		// requiredRole, but role=supervisor needs an admin
		{"role held, gate applies and fails", Policy{RequiredRole: []string{"supervisor", "admin"}, FieldGatedRole: gate}, "supervisor", "", `{"role":"supervisor"}`, false},
		{"role held, gate applies and holds", Policy{RequiredRole: []string{"supervisor", "admin"}, FieldGatedRole: gate}, "admin", "", `{"role":"supervisor"}`, true},
		{"role held, gate doesn't apply", Policy{RequiredRole: []string{"supervisor", "admin"}, FieldGatedRole: gate}, "supervisor", "", `{"role":"member"}`, true},
		// a field gate that doesn't apply must not end the evaluation: scope
		// still decides (m1 is a member of north, u2 isn't)
		{"gate doesn't apply, not in scope", Policy{FieldGatedRole: gate, Scope: scope}, "member", "u2", `{"role":"member"}`, false},
		{"gate doesn't apply, in scope", Policy{FieldGatedRole: gate, Scope: scope}, "member", "m1", `{"role":"member"}`, true},
		// ownership + scope: the owner who isn't a member of the region
		{"owner, not in scope", Policy{Ownership: ownership, Scope: scope}, "member", "u1", `{}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Authorizer{
				Policies:            map[Key]Policy{{"order", "UpdateOrder"}: c.policy},
				ResolveOwnRole:      func(*core.Record) string { return c.role },
				ResolveOwnSubjectID: func(*core.Record) string { return c.subjectID },
			}
			ok, err := a.Authorize(app, "order", "UpdateOrder", "o1", []byte(c.payload), nil)
			if err != nil {
				t.Fatal(err)
			}
			if ok != c.want {
				t.Fatalf("got %v, want %v", ok, c.want)
			}
		})
	}
}
