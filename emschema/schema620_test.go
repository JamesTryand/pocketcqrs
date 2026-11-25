package emschema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Schema 6.2.0: a scenario whose `then` lists no events, a system's `expects`,
// and the fields a public event publishes.

// formDoc has one command that is accepted and records nothing when n is more
// than 1, and records form-opened otherwise. scenarios is its scenario list.
func formDoc(scenarios string) string {
	return `{"eventModelingSchemaVersion":"6.2.0","id":"f","name":"F",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{"form-opened":{"name":"Form Opened","swimlaneId":"s","aggregate":"form",
		"fields":[{"name":"formId","type":"string","idAttribute":true},{"name":"n","type":"decimal"}]}},
	"commands":{"open-form":{"name":"Open Form","aggregate":"form",
		"fields":[{"name":"formId","type":"string","idAttribute":true},{"name":"n","type":"decimal"}],
		"refuseWhen":[{"message":"a negative count is refused","all":[{"command":"n","lessThan":0}]}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[{"id":"open","name":"Open","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc",
		"commandId":"open-form","eventIds":["form-opened"],
		"outcomes":[{"eventIds":[],"when":[{"all":[{"command":"n","greaterThan":1}]}]},["form-opened"]],
		"scenarios":` + scenarios + `}]}`
}

const nothingRecordedScenarios = `[
	{"id":"means-nothing","name":"accepted, nothing recorded, the stream never opened","kind":"stateChange","given":[],
	 "when":{"commandId":"open-form","data":{"formId":"f1","n":5}},"then":{"events":[]}},
	{"id":"records-something","name":"says nothing is recorded, but something is","kind":"stateChange","given":[],
	 "when":{"commandId":"open-form","data":{"formId":"f1","n":0}},"then":{"events":[]}},
	{"id":"is-refused","name":"says nothing is recorded, but is refused","kind":"stateChange","given":[],
	 "when":{"commandId":"open-form","data":{"formId":"f1","n":-1}},"then":{"events":[]}}]`

// An empty `then` is an assertion: it passes only when the command is
// accepted and records exactly nothing. Run through the generated JS and the
// generated Go.
func TestAnEmptyThenPassesOnlyWhenNothingIsRecorded(t *testing.T) {
	doc, err := Load(writeDoc(t, formDoc(nothingRecordedScenarios)))
	if err != nil {
		t.Fatal(err)
	}
	if rep := Lint(doc); len(rep.Errors) > 0 {
		t.Fatalf("an empty then is not a lint error: %v", rep.Errors)
	}
	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	js, err := Verify(doc, mapped, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ScenarioResult{}
	for _, r := range js {
		byID[r.ScenarioID] = r
	}
	for name, results := range map[string]map[string]ScenarioResult{"js": byID, "go": goVerify(t, doc, mapped, "")} {
		if r := results["means-nothing"]; !r.Passed || !strings.Contains(r.Detail, "nothing recorded") {
			t.Errorf("%s: accepted with nothing recorded should pass, and say so: %+v", name, r)
		}
		if r := results["records-something"]; r.Passed || !strings.Contains(r.Detail, "FormOpened") {
			t.Errorf("%s: a command that records an event fails an empty then, naming it: %+v", name, r)
		}
		if r := results["is-refused"]; r.Passed || !strings.Contains(r.Detail, "refused") {
			t.Errorf("%s: a refusal fails an empty then: %+v", name, r)
		}
	}
}

// A `then` with no `events` at all is not "nothing recorded": it says nothing,
// so it is refused rather than read as an empty list.
func TestAThenWithoutEventsIsRefused(t *testing.T) {
	sc := Scenario{ID: "x", Kind: KindStateChange, Then: []byte(`{}`)}
	if _, err := sc.EventsThen(); err == nil || !strings.Contains(err.Error(), "no `events`") {
		t.Fatalf("want a then without events refused, got %v", err)
	}
	sc.Then = []byte(`{"events":[]}`)
	then, err := sc.EventsThen()
	if err != nil || then.Events == nil || len(then.Events) != 0 {
		t.Fatalf("want an empty list read as one, got %+v, %v", then, err)
	}
}

const expectsSection = `"expects":{"library-members":{"writtenAgainst":"v1.0.0","description":"joins become borrowers",
	"events":[{"event":"member-joined","version":1,
		"fields":[{"name":"memberId","type":"string"},{"name":"branchId","type":"string","cardinality":"single"}]}]}}`

// `expects` is read in full (every key of it is one this importer knows) and
// generates nothing: an expected event is never this system's own.
func TestExpectsIsReadAndGeneratesNothing(t *testing.T) {
	body := strings.Replace(formDoc(`[]`), `"screens":`, expectsSection+`,"screens":`, 1)
	doc, err := Load(writeDoc(t, body))
	if err != nil {
		t.Fatal(err)
	}
	exp, ok := doc.Expects["library-members"]
	if !ok || exp.WrittenAgainst != "v1.0.0" || len(exp.Events) != 1 || exp.Events[0].Event != "member-joined" ||
		len(exp.Events[0].Fields) != 2 || exp.Events[0].Fields[1].Cardinality != "single" {
		t.Fatalf("expects was not read: %+v", doc.Expects)
	}
	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(mapped.Domains) != 1 || mapped.Domains[0].Aggregate != "form" || len(mapped.Domains[0].Folds) != 0 {
		t.Errorf("an expected event was generated as this system's own: %+v", mapped.Domains)
	}
	// a key inside it that this importer does not read is still refused
	p := writeDoc(t, strings.Replace(body, `"version":1,`, `"version":1,"via":"a-queue",`, 1))
	wantRefused(t, refusedAt(t, p), "/expects/library-members/events/0/via")
}

// A split document carries `expects` in its manifest.
func TestASplitDocumentKeepsItsExpects(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", `{"eventModelingSchemaVersion":"6.2.0","id":"f","name":"F",
		"swimlanes":[{"id":"s","name":"S","kind":"team"}],`+expectsSection+`,
		"registries":{"events":"registries/events.json","commands":"registries/commands.json","screens":"registries/screens.json"},
		"slices":["slices/open.json"]}`)
	write("registries/events.json", `{"form-opened":{"name":"Form Opened","swimlaneId":"s","aggregate":"form",
		"fields":[{"name":"formId","type":"string","idAttribute":true}]}}`)
	write("registries/commands.json", `{"open-form":{"name":"Open Form","aggregate":"form",
		"fields":[{"name":"formId","type":"string","idAttribute":true}]}}`)
	write("registries/screens.json", `{"sc":{"name":"Sc"}}`)
	write("slices/open.json", `{"id":"open","name":"Open","pattern":"stateChange","swimlaneId":"s","status":"planned",
		"screenId":"sc","commandId":"open-form","eventIds":["form-opened"],"scenarios":[]}`)
	doc, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Expects["library-members"].Events) != 1 {
		t.Fatalf("the manifest's expects was dropped: %+v", doc.Expects)
	}
}

// A system never expects from itself.
func TestASystemExpectingFromItselfIsALintError(t *testing.T) {
	body := strings.Replace(formDoc(`[]`), `"screens":`,
		strings.Replace(expectsSection, `"library-members"`, `"f"`, 1)+`,"screens":`, 1)
	doc, err := Load(writeDoc(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if rep := Lint(doc); !strings.Contains(strings.Join(rep.Errors, "\n"), "expects from itself") {
		t.Fatalf("want the lint to refuse it, got %v", rep.Errors)
	}
}

// An event's public marker may list the fields it publishes (a command's may
// not), and each must be one the event declares.
func TestAPublicEventListsItsFields(t *testing.T) {
	marker := `"public":{"version":1,"fields":["formId"]},`
	body := strings.Replace(formDoc(`[]`), `"aggregate":"form",
		"fields":[{"name":"formId","type":"string","idAttribute":true},{"name":"n","type":"decimal"}]}},
	"commands"`, `"aggregate":"form",`+marker+`
		"fields":[{"name":"formId","type":"string","idAttribute":true},{"name":"n","type":"decimal"}]}},
	"commands"`, 1)
	doc, err := Load(writeDoc(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if pub := doc.Events["form-opened"].Public; pub == nil || strings.Join(pub.Fields, ",") != "formId" {
		t.Fatalf("the published fields were not read: %+v", pub)
	}
	if rep := Lint(doc); len(rep.Errors) > 0 {
		t.Fatalf("a declared field is fine: %v", rep.Errors)
	}
	doc, err = Load(writeDoc(t, strings.Replace(body, `"fields":["formId"]`, `"fields":["formId","colour"]`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if rep := Lint(doc); !strings.Contains(strings.Join(rep.Errors, "\n"), `"colour"`) {
		t.Fatalf("want a listed field the event does not declare refused, got %v", rep.Errors)
	}
	p := writeDoc(t, strings.Replace(body, `"commands":{"open-form":{"name":"Open Form",`,
		`"commands":{"open-form":{"name":"Open Form","public":{"version":1,"fields":["formId"]},`, 1))
	wantRefused(t, refusedAt(t, p), "/commands/open-form/public/fields")
}

// The schema's own example of two systems in their own repositories, in its
// real format: lending states what it expects of members and has a scenario
// accepted with nothing recorded; members lists the fields it publishes.
func TestTheLinksExamplesImportAndVerify(t *testing.T) {
	for _, rel := range []string{"examples/links/lending/lending.eventmodel.json", "examples/links/members/members.eventmodel.json"} {
		doc, err := Load(fixture(t, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if rep := Lint(doc); len(rep.Errors) > 0 {
			t.Fatalf("%s: %v", rel, rep.Errors)
		}
		mapped, err := Map(doc, Options{})
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		results, err := Verify(doc, mapped, t.TempDir())
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		for _, r := range results {
			if !r.Passed {
				t.Errorf("%s: %s: %s", rel, r.ScenarioID, r.Detail)
			}
			if r.ScenarioID == "the-reading-room-lends-nothing" && r.Detail != "accepted, nothing recorded" {
				t.Errorf("the reading room's scenario: %s", r.Detail)
			}
		}
	}
}

// membersWithFields is membersDoc with member-joined's marker listing fields.
func membersWithFields(fields string) string {
	doc := strings.Replace(membersDoc, `"public": { "version": 1 }`, `"public": { "version": 1, "fields": [`+fields+`] }`, 1)
	return strings.Replace(doc, `"eventModelingSchemaVersion": "6.1.0"`, `"eventModelingSchemaVersion": "6.2.0"`, 1)
}

// edgesOfSender is edgesOf for the members system, with its own document
// given.
func edgesOfSender(t *testing.T, members, edge string) (*SystemEdges, error) {
	t.Helper()
	arch, _ := archDocs(t, edge)
	docPath := filepath.Join(filepath.Dir(arch), "members.eventmodel.json")
	if err := os.WriteFile(docPath, []byte(members), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := LoadArchitecture(arch)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Load(docPath)
	if err != nil {
		t.Fatal(err)
	}
	return EdgesFor(a, "members", doc, docPath, Options{})
}

// With a list, the sender publishes what it lists, not only what this
// document's edges map: another view's link may read the rest.
func TestASenderPublishesWhatItsMarkerLists(t *testing.T) {
	onlyID := strings.Replace(memberBecomesBorrower, `"fields": { "borrowerId": "memberId", "branchId": "branchId" }`,
		`"fields": { "borrowerId": "memberId" }`, 1)
	onlyID = strings.Replace(onlyID, `"fields": { "borrowerId": "m-1", "branchId": "north" }`, `"fields": { "borrowerId": "m-1" }`, 1)
	se, err := edgesOfSender(t, membersWithFields(`"memberId", "branchId"`), onlyID)
	if err != nil {
		t.Fatal(err)
	}
	if len(se.Out) != 1 || strings.Join(se.Out[0].Fields, ",") != "branchId,memberId" {
		t.Fatalf("want both listed fields published, got %+v", se.Out)
	}
}

func TestAnEdgeReadingAFieldNotListedIsRefused(t *testing.T) {
	_, err := edgesOfSender(t, membersWithFields(`"memberId"`), memberBecomesBorrower)
	if err == nil || !strings.Contains(err.Error(), `"branchId"`) || !strings.Contains(err.Error(), "does not publish") {
		t.Fatalf("want the edge reading branchId refused, got %v", err)
	}
}

func TestAListedPersonalFieldIsRefused(t *testing.T) {
	_, err := edgesOfSender(t, membersWithFields(`"memberId", "branchId", "email"`), memberBecomesBorrower)
	if err == nil || !strings.Contains(err.Error(), `"email" is personal data`) {
		t.Fatalf("want the listed personal field refused, got %v", err)
	}
}
