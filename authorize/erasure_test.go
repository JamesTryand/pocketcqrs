package authorize

import (
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/emschema"
	"github.com/jamestryand/pocketcqrs/pii"
)

// dataSubjects.erasure (schema 3.2.0, 3.7.1) admits RequestErasure only:
// self for the caller's own subject id (or, with via, the subject ids a read
// model says they own), roles for any subject. Every other dataSubject
// command is refused, and so is every dataSubject command when the document
// declares nothing: the built-in aggregate is the one place "no policy" does
// not mean "allowed".

func erasureDoc(e *emschema.ErasureAuthorization) *emschema.Document {
	doc := &emschema.Document{ReadModels: map[string]emschema.ReadModel{"customers": {Name: "Customers"}}}
	if e != nil {
		doc.DataSubjects = &emschema.DataSubjects{Erasure: e}
	}
	return doc
}

func erasureAuthorizer(t *testing.T, e *emschema.ErasureAuthorization, role, subjectID *string) *Authorizer {
	t.Helper()
	rule, err := BuildErasureRequest(erasureDoc(e))
	if err != nil {
		t.Fatal(err)
	}
	return &Authorizer{
		Policies:            map[Key]Policy{},
		ErasureRequest:      rule,
		ResolveOwnRole:      func(*core.Record) string { return *role },
		ResolveOwnSubjectID: func(*core.Record) string { return *subjectID },
	}
}

type erasureCase struct {
	name, role, caller, aggregate, command, subject string
	want                                            bool
}

func runErasureCases(t *testing.T, app core.App, a *Authorizer, role, subjectID *string, cases []erasureCase) {
	t.Helper()
	for _, c := range cases {
		*role, *subjectID = c.role, c.caller
		got, err := a.Authorize(app, c.aggregate, c.command, c.subject, nil, nil)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: expected %v, got %v", c.name, c.want, got)
		}
	}
}

func TestAuthorize_ErasureSelfAndRoles(t *testing.T) {
	var role, subjectID string
	a := erasureAuthorizer(t, &emschema.ErasureAuthorization{Self: true, Roles: []string{"support"}}, &role, &subjectID)
	ds, req := pii.SubjectAggregate, pii.RequestErasureCommand
	runErasureCases(t, newTestApp(t), a, &role, &subjectID, []erasureCase{
		{"self: a customer requests their own erasure", "customer", "c-old", ds, req, "c-old", true},
		{"self: another customer's is refused", "customer", "c-old", ds, req, "c-other", false},
		{"self: a caller with no subject id is refused", "customer", "", ds, req, "", false},
		{"roles: support requests anyone's", "support", "agent-1", ds, req, "c-other", true},
		{"roles are matched case-insensitively", "SUPPORT", "agent-1", ds, req, "c-other", true},
		{"the aggregate is matched as the gateway matches it", "support", "agent-1", "DataSubject", req, "c-other", true},
		{"never the erasure itself, even for a listed role", "support", "agent-1", ds, pii.EraseSubjectCommand, "c-other", false},
		{"never the erasure itself, even for oneself", "customer", "c-old", ds, pii.EraseSubjectCommand, "c-old", false},
		{"never a governance command", "support", "agent-1", ds, "ApproveErasure", "c-other", false},
		{"a document command with no policy is still allowed", "customer", "c-old", "customer", "RegisterCustomer", "c-old", true},
	})
}

func TestAuthorize_ErasureSelfThroughVia(t *testing.T) {
	app := newTestApp(t)
	// c-old and c-new both belong to login-1 (a returning person gets a new
	// subject id); c-other to login-2
	col := mustCreateCollection(t, app, "customers", "customerId", "loginId")
	mustInsertRow(t, app, col, map[string]string{"customerId": "c-old", "loginId": "login-1"})
	mustInsertRow(t, app, col, map[string]string{"customerId": "c-new", "loginId": "login-1"})
	mustInsertRow(t, app, col, map[string]string{"customerId": "c-other", "loginId": "login-2"})

	var role, subjectID string
	a := erasureAuthorizer(t, &emschema.ErasureAuthorization{Self: true,
		Via: &emschema.CommandOwnershipVia{ReadModelID: "customers", KeyField: "customerId", OwnerField: "loginId"}}, &role, &subjectID)
	ds, req := pii.SubjectAggregate, pii.RequestErasureCommand
	runErasureCases(t, app, a, &role, &subjectID, []erasureCase{
		{"via: login-1 requests its earlier subject's erasure", "customer", "login-1", ds, req, "c-old", true},
		{"via: and its current one's", "customer", "login-1", ds, req, "c-new", true},
		{"via: not another login's", "customer", "login-1", ds, req, "c-other", false},
		{"via: the login id itself is not a subject id", "customer", "login-1", ds, req, "login-1", false},
		{"via: no roles were declared, so a role admits nothing", "support", "agent-1", ds, req, "c-old", false},
	})
}

func TestAuthorize_ErasureWithoutADeclaration(t *testing.T) {
	var role, subjectID string
	a := erasureAuthorizer(t, nil, &role, &subjectID)
	if a.ErasureRequest != nil {
		t.Fatalf("a document declaring no dataSubjects must build no erasure rule, got %+v", a.ErasureRequest)
	}
	ds, req := pii.SubjectAggregate, pii.RequestErasureCommand
	runErasureCases(t, newTestApp(t), a, &role, &subjectID, []erasureCase{
		{"no declaration: not oneself", "customer", "c-old", ds, req, "c-old", false},
		{"no declaration: not any role", "administrator", "admin-1", ds, req, "c-old", false},
		{"no declaration: never the erasure", "administrator", "admin-1", ds, pii.EraseSubjectCommand, "c-old", false},
	})
}

func TestBuildErasureRequestRefusesADanglingVia(t *testing.T) {
	doc := erasureDoc(&emschema.ErasureAuthorization{Self: true,
		Via: &emschema.CommandOwnershipVia{ReadModelID: "accounts", KeyField: "customerId", OwnerField: "loginId"}})
	if _, err := BuildErasureRequest(doc); err == nil || !strings.Contains(err.Error(), `"accounts"`) {
		t.Fatalf("a dangling dataSubjects.erasure.via must be refused, got %v", err)
	}
}
