package emschema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dataSubjects.erasure (schema 3.2.0, 3.7.1): imported, linted, and resolved
// to the policy package authorize evaluates.

func erasureLintDoc(e *ErasureAuthorization, pii bool) *Document {
	return &Document{
		Swimlanes: []Swimlane{{ID: "s", Name: "S", Kind: "team"}},
		Events: map[string]Event{"customer-registered": {Name: "Customer Registered", SwimlaneID: "s",
			Fields: []Field{{Name: "email", Type: "string", PII: pii, PIISubject: "customerId"}}}},
		ReadModels:   map[string]ReadModel{"customers": {Name: "Customers"}},
		DataSubjects: &DataSubjects{Erasure: e},
	}
}

func lintMentions(msgs []string, parts ...string) bool {
	for _, m := range msgs {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(m, p)
		}
		if all {
			return true
		}
	}
	return false
}

func TestLintDataSubjectsErasure(t *testing.T) {
	via := &CommandOwnershipVia{ReadModelID: "customers", KeyField: "customerId", OwnerField: "loginId"}
	cases := []struct {
		name  string
		e     *ErasureAuthorization
		pii   bool
		error []string
		warn  []string
	}{
		{"self and roles", &ErasureAuthorization{Self: true, Roles: []string{"support"}}, true, nil, nil},
		{"self through via", &ErasureAuthorization{Self: true, Via: via}, true, nil, nil},
		{"neither", &ErasureAuthorization{}, true, []string{"dataSubjects.erasure", "neither self nor roles"}, nil},
		{"via without self", &ErasureAuthorization{Roles: []string{"support"}, Via: via}, true,
			[]string{"dataSubjects.erasure.via", "self: true"}, nil},
		{"dangling via", &ErasureAuthorization{Self: true,
			Via: &CommandOwnershipVia{ReadModelID: "accounts", KeyField: "customerId", OwnerField: "loginId"}}, true,
			[]string{"dataSubjects.erasure.via", `"accounts"`}, nil},
		{"no personal data", &ErasureAuthorization{Self: true}, false, nil, []string{"dataSubjects.erasure", "pii"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &Report{}
			lintDataSubjects(erasureLintDoc(c.e, c.pii), r)
			if c.error == nil && len(r.Errors) > 0 {
				t.Errorf("unexpected errors: %v", r.Errors)
			}
			if c.error != nil && !lintMentions(r.Errors, c.error...) {
				t.Errorf("no error mentioning %q: %v", c.error, r.Errors)
			}
			if c.warn != nil && !lintMentions(r.Warnings, c.warn...) {
				t.Errorf("no warning mentioning %q: %v", c.warn, r.Warnings)
			}
		})
	}
}

func TestErasureRequestPolicy(t *testing.T) {
	doc := erasureLintDoc(&ErasureAuthorization{Self: true, Roles: []string{"support"},
		Via: &CommandOwnershipVia{ReadModelID: "customers", KeyField: "customerId", OwnerField: "loginId"}}, true)
	rule, err := ErasureRequestPolicy(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !rule.Self || len(rule.Roles) != 1 || rule.Roles[0] != "support" || rule.Via == nil ||
		rule.Via.Collection != "customers" || rule.Via.KeyField != "customerId" || rule.Via.OwnerField != "loginId" {
		t.Fatalf("unexpected rule: %+v (via %+v)", rule, rule.Via)
	}

	doc.DataSubjects = nil
	if rule, err := ErasureRequestPolicy(doc); err != nil || rule != nil {
		t.Fatalf("no declaration must resolve to no rule, got %+v, %v", rule, err)
	}
}

// The manifest carries dataSubjects inline (schema 3.2.0), so a split
// document must keep it through the join.
func TestSplitDocumentCarriesDataSubjects(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", `{"eventModelingSchemaVersion":"4.1.0","id":"d","name":"D",
		"swimlanes":[{"id":"s","name":"S","kind":"team"}],
		"dataSubjects":{"erasure":{"self":true,"roles":["support"]}},
		"registries":{"commands":"commands.json"},
		"slices":[]}`)
	write("commands.json", `{"c":{"name":"C"}}`)
	doc, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e := doc.DataSubjects.erasureOrNil(); e == nil || !e.Self || len(e.Roles) != 1 || e.Roles[0] != "support" {
		t.Fatalf("the manifest's dataSubjects was lost in the join: %+v", doc.DataSubjects)
	}
}
