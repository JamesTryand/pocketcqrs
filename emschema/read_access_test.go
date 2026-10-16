package emschema

import (
	"strings"
	"testing"
)

func readAccessDoc(subjectField string) string {
	return `{"eventModelingSchemaVersion":"3.17.0","id":"r","name":"R",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{"ticket-opened":{"name":"Ticket Opened","swimlaneId":"s","aggregate":"ticket",
		"fields":[{"name":"ticketId","type":"string","idAttribute":true},{"name":"agentId","type":"string"},{"name":"teamId","type":"string"}]}},
	"commands":{"open-ticket":{"name":"Open Ticket","aggregate":"ticket","fields":[{"name":"ticketId","type":"string","idAttribute":true}]}},
	"readModels":{
		"tickets":{"name":"Tickets","builtFromEventIds":["ticket-opened"],"requiredRole":"supervisor",
			"fields":[{"name":"ticketId","type":"string"},{"name":"agentId","type":"string"},{"name":"teamId","type":"string"}],
			"selfAccess":{"subjectField":"` + subjectField + `","param":"mine"},
			"scopes":[{"param":"leadAgentId","grantsAccess":true,
				"via":{"readModelId":"team-leads","matchParamTo":"agentId","selectField":"teamId","filterLocalField":"teamId"}}]},
		"team-leads":{"name":"Team Leads","builtFromEventIds":["ticket-opened"],
			"fields":[{"name":"ticketId","type":"string"},{"name":"agentId","type":"string"},{"name":"teamId","type":"string"}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[{"id":"log","name":"Log","pattern":"stateChange","swimlaneId":"s","status":"planned",
		"screenId":"sc","commandId":"open-ticket","eventIds":["ticket-opened"],"scenarios":[]}]}`
}

// selfAccess and a granting scope reach the generated JS rule, with the
// caller's fields named at import, and a read model with no access rule is
// signed-in only.
func TestReadAccessReachesTheGeneratedRule(t *testing.T) {
	doc, err := Load(writeDoc(t, readAccessDoc("agentId")))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{RoleField: "jobRole", SubjectField: "agentId"})
	if err != nil {
		t.Fatal(err)
	}
	files, err := mapped.Domains[0].Generate()
	if err != nil {
		t.Fatal(err)
	}
	var tickets, leads string
	for _, f := range files {
		switch f.Name {
		case "tickets.js":
			tickets = f.Source
		case "teamLeads.js":
			leads = f.Source
		}
	}
	for _, want := range []string{
		`@request.auth.jobRole = "supervisor"`,
		`(@request.auth.agentId != "" && agentId = @request.auth.agentId)`,
		`@collection.teamLeads:grant0.agentId ?= @request.auth.agentId && @collection.teamLeads:grant0.teamId ?= teamId`,
		`"mine" narrows a caller to their own rows`,
	} {
		if !strings.Contains(tickets, want) {
			t.Errorf("tickets.js lacks %q:\n%s", want, tickets)
		}
	}
	if !strings.Contains(leads, "//@rule teamLeads authenticated") {
		t.Errorf("a read model with no access rule is signed-in only:\n%s", leads)
	}

	// the Go target's read route carries the same rule
	goFiles, err := mapped.Domains[0].GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	var route string
	for _, f := range goFiles {
		if f.Name == "tickets_query.go" {
			route = f.Source
		}
	}
	for _, want := range []string{
		`RequiredRole: []string{"supervisor"}`,
		`SelfSubject: "agentId"`,
		`SelfParam: "mine"`,
		`{Collection: "teamLeads", MatchField: "agentId", SelectField: "teamId", LocalField: "teamId"}`,
	} {
		// gofmt aligns the fields, so compare with spacing collapsed
		if !strings.Contains(strings.Join(strings.Fields(route), " "), want) {
			t.Errorf("tickets_query.go lacks %q:\n%s", want, route)
		}
	}
}

// A selfAccess subject that isn't one of the read model's own fields is
// refused: a rule naming a missing column would match nothing, or the
// wrong thing.
func TestSelfAccessSubjectMustBeAField(t *testing.T) {
	doc, err := Load(writeDoc(t, readAccessDoc("ownerId")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Map(doc, Options{}); err == nil || !strings.Contains(err.Error(), "ownerId") {
		t.Fatalf("want a refusal naming ownerId, got %v", err)
	}
}
