package emschema

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/functions"
)

func authorizeDoc() string {
	return `{"eventModelingSchemaVersion":"3.17.0","id":"a","name":"A",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{
		"ticket-opened":{"name":"Ticket Opened","swimlaneId":"s","aggregate":"ticket",
			"fields":[{"name":"ticketId","type":"string","idAttribute":true},{"name":"teamId","type":"string"},{"name":"priority","type":"string"}]},
		"ticket-escalated":{"name":"Ticket Escalated","swimlaneId":"s","aggregate":"ticket",
			"fields":[{"name":"ticketId","type":"string","idAttribute":true}]},
		"ticket-noted":{"name":"Ticket Noted","swimlaneId":"s","aggregate":"ticket",
			"fields":[{"name":"ticketId","type":"string","idAttribute":true}]}},
	"commands":{
		"open-ticket":{"name":"Open Ticket","aggregate":"ticket",
			"fields":[{"name":"ticketId","type":"string","idAttribute":true},{"name":"teamId","type":"string"},{"name":"priority","type":"string"}],
			"fieldGatedRole":{"field":"priority","value":"urgent","requiredRole":"supervisor"}},
		"escalate-ticket":{"name":"Escalate Ticket","aggregate":"ticket","requiredRole":["supervisor","admin"],
			"fields":[{"name":"ticketId","type":"string","idAttribute":true}],
			"scope":{"bypassRoles":["admin"],"message":"only the ticket's team may escalate it",
				"resolveVia":{"readModelId":"tickets","keyField":"ticketId","selectField":"teamId"},
				"memberOfVia":{"readModelId":"team-members","matchField":"teamId","actorField":"memberId"}}},
		"note-ticket":{"name":"Note Ticket","aggregate":"ticket",
			"fields":[{"name":"ticketId","type":"string","idAttribute":true}]}},
	"readModels":{
		"tickets":{"name":"Tickets","builtFromEventIds":["ticket-opened"],
			"fields":[{"name":"ticketId","type":"string","idAttribute":true},{"name":"teamId","type":"string"}]},
		"team-members":{"name":"Team Members","builtFromEventIds":["ticket-opened"],
			"fields":[{"name":"ticketId","type":"string","idAttribute":true},{"name":"teamId","type":"string"},{"name":"memberId","type":"string"}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"open","name":"Open","pattern":"stateChange","swimlaneId":"s","status":"planned",
			"screenId":"sc","commandId":"open-ticket","eventIds":["ticket-opened"],"scenarios":[]},
		{"id":"escalate","name":"Escalate","pattern":"stateChange","swimlaneId":"s","status":"planned",
			"screenId":"sc","commandId":"escalate-ticket","eventIds":["ticket-escalated"],"scenarios":[]},
		{"id":"note","name":"Note","pattern":"stateChange","swimlaneId":"s","status":"planned",
			"screenId":"sc","commandId":"note-ticket","eventIds":["ticket-noted"],"scenarios":[]}]}`
}

// The permissions a model declares reach the running server unchanged: the
// generated decider's //@authorize lines load back into exactly the table
// verify checks scenarios against (CommandPolicies), and a command that
// declares nothing gets no line, so it is not checked.
func TestCommandPermissionsReachTheLoadedDecider(t *testing.T) {
	doc, err := Load(writeDoc(t, authorizeDoc()))
	if err != nil {
		t.Fatal(err)
	}
	want, err := CommandPolicies(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 2 {
		t.Fatalf("the fixture should declare two policies, got %d: %v", len(want), want)
	}

	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := mapped.Domains[0].Generate()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var decider string
	for _, f := range files {
		if f.Kind == "decider" {
			decider = f.Source
		}
		if err := os.WriteFile(filepath.Join(dir, f.Name), []byte(f.Source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(decider, "//@authorize "); got != 2 {
		t.Errorf("want two //@authorize lines, got %d:\n%s", got, decider)
	}
	if strings.Contains(decider, "//@authorize NoteTicket") {
		t.Errorf("a command declaring nothing must get no //@authorize line:\n%s", decider)
	}

	loaded, err := functions.LoadDir(functions.NewGojaRuntime(nil), nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	specs := map[string]*functions.DeciderSpec{}
	for _, s := range loaded.Deciders {
		specs[s.Aggregate] = s
	}
	got := functions.CommandPolicies(specs)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loaded table differs from the model's:\n got  %#v\n want %#v", got, want)
	}
}

// A model with no command permissions generates no //@authorize line at all:
// its deciders load to an empty table, which allows every command.
func TestNoPermissionsMeansNoDirective(t *testing.T) {
	doc, err := Load(writeDoc(t, readAccessDoc("agentId")))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := mapped.Domains[0].Generate()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.Contains(f.Source, "//@authorize") {
			t.Errorf("%s has an //@authorize line though the model declares none:\n%s", f.Name, f.Source)
		}
	}
	if p, err := CommandPolicies(doc); err != nil || len(p) != 0 {
		t.Fatalf("fixture should declare no policy: %v %v", p, err)
	}
}
