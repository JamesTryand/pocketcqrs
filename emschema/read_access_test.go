package emschema

import (
	"strings"
	"testing"
)

func readAccessDoc(subjectField string) string {
	return `{"eventModelingSchemaVersion":"3.17.0","id":"r","name":"R",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{"entry-logged":{"name":"Entry Logged","swimlaneId":"s","aggregate":"entry",
		"fields":[{"name":"entryId","type":"string","idAttribute":true},{"name":"staffId","type":"string"},{"name":"projectId","type":"string"}]}},
	"commands":{"log-entry":{"name":"Log Entry","aggregate":"entry","fields":[{"name":"entryId","type":"string","idAttribute":true}]}},
	"readModels":{
		"time-entries":{"name":"Time Entries","builtFromEventIds":["entry-logged"],"requiredRole":"manager",
			"fields":[{"name":"entryId","type":"string"},{"name":"staffId","type":"string"},{"name":"projectId","type":"string"}],
			"selfAccess":{"subjectField":"` + subjectField + `","param":"mine"},
			"scopes":[{"param":"pmStaffId","grantsAccess":true,
				"via":{"readModelId":"project-managers","matchParamTo":"staffId","selectField":"projectId","filterLocalField":"projectId"}}]},
		"project-managers":{"name":"Project Managers","builtFromEventIds":["entry-logged"],
			"fields":[{"name":"entryId","type":"string"},{"name":"staffId","type":"string"},{"name":"projectId","type":"string"}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[{"id":"log","name":"Log","pattern":"stateChange","swimlaneId":"s","status":"planned",
		"screenId":"sc","commandId":"log-entry","eventIds":["entry-logged"],"scenarios":[]}]}`
}

// selfAccess and a granting scope reach the generated JS rule, with the
// caller's fields named at import (decision 6), and a read model with no
// access rule is signed-in only (decision 8).
func TestReadAccessReachesTheGeneratedRule(t *testing.T) {
	doc, err := Load(writeDoc(t, readAccessDoc("staffId")))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{RoleField: "jobRole", SubjectField: "staffId"})
	if err != nil {
		t.Fatal(err)
	}
	files, err := mapped.Domains[0].Generate()
	if err != nil {
		t.Fatal(err)
	}
	var entries, managers string
	for _, f := range files {
		switch f.Name {
		case "timeEntries.js":
			entries = f.Source
		case "projectManagers.js":
			managers = f.Source
		}
	}
	for _, want := range []string{
		`@request.auth.jobRole = "manager"`,
		`(@request.auth.staffId != "" && staffId = @request.auth.staffId)`,
		`@collection.projectManagers:grant0.staffId ?= @request.auth.staffId && @collection.projectManagers:grant0.projectId ?= projectId`,
		`"mine" narrows a caller to their own rows`,
	} {
		if !strings.Contains(entries, want) {
			t.Errorf("timeEntries.js lacks %q:\n%s", want, entries)
		}
	}
	if !strings.Contains(managers, "//@rule projectManagers authenticated") {
		t.Errorf("a read model with no access rule is signed-in only:\n%s", managers)
	}

	// fail closed on the Go target, which doesn't apply these yet
	if _, err := mapped.Domains[0].GenerateGo(); err == nil || !strings.Contains(err.Error(), "selfAccess") {
		t.Errorf("the Go target must refuse selfAccess/grantsAccess, got %v", err)
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
