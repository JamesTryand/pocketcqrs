package emschema

import (
	"strings"
	"testing"
)

// callerDoc: time entries (managers read all, staff their own, a project
// manager also those on their projects) and the project-manager
// assignments the granting scope reads through. Its view scenarios each
// read as a different caller; "wrong" expects what the rule does NOT
// allow, so it must fail.
var callerDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"c","name":"C",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{
		"entry-logged":{"name":"Entry Logged","swimlaneId":"s","aggregate":"entry",
			"fields":[{"name":"entryId","type":"string","idAttribute":true},{"name":"staffId","type":"string"},{"name":"projectId","type":"string"}]},
		"pm-assigned":{"name":"Pm Assigned","swimlaneId":"s","aggregate":"assignment",
			"fields":[{"name":"assignmentId","type":"string","idAttribute":true},{"name":"staffId","type":"string"},{"name":"projectId","type":"string"}]}},
	"commands":{
		"log-entry":{"name":"Log Entry","aggregate":"entry","fields":[{"name":"entryId","type":"string","idAttribute":true}]},
		"assign-pm":{"name":"Assign Pm","aggregate":"assignment","fields":[{"name":"assignmentId","type":"string","idAttribute":true}]}},
	"readModels":{
		"time-entries":{"name":"Time Entries","builtFromEventIds":["entry-logged"],"requiredRole":"manager",
			"fields":[{"name":"entryId","type":"string","idAttribute":true},{"name":"staffId","type":"string"},{"name":"projectId","type":"string"}],
			"selfAccess":{"subjectField":"staffId","param":"mine"},
			"scopes":[{"param":"pmStaffId","grantsAccess":true,
				"via":{"readModelId":"project-managers","matchParamTo":"staffId","selectField":"projectId","filterLocalField":"projectId"}}]},
		"project-managers":{"name":"Project Managers","builtFromEventIds":["pm-assigned"],"requiredRole":"manager",
			"fields":[{"name":"assignmentId","type":"string","idAttribute":true},{"name":"staffId","type":"string"},{"name":"projectId","type":"string"}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"log","name":"Log","pattern":"stateChange","swimlaneId":"s","status":"planned",
		 "screenId":"sc","commandId":"log-entry","eventIds":["entry-logged"],"scenarios":[]},
		{"id":"assign","name":"Assign","pattern":"stateChange","swimlaneId":"s","status":"planned",
		 "screenId":"sc","commandId":"assign-pm","eventIds":["pm-assigned"],"scenarios":[]},
		{"id":"view","name":"View","pattern":"stateView","swimlaneId":"s","status":"planned",
		 "screenId":"sc","readModelId":"time-entries","scenarios":[
			` + callerScenario("manager", `{"subjectId":"m1","role":"manager"}`, `null`, `["e1","e2","e3"]`) + `,
			` + callerScenario("own", `{"subjectId":"s2","role":"staff"}`, `null`, `["e2"]`) + `,
			` + callerScenario("pm", `{"subjectId":"s9","role":"staff"}`, `null`, `["e1","e2"]`) + `,
			` + callerScenario("pm-param-ignored", `{"subjectId":"s9","role":"staff"}`, `{"pmStaffId":"s1"}`, `["e1","e2"]`) + `,
			` + callerScenario("mine", `{"subjectId":"m1","role":"manager"}`, `{"mine":true}`, `[]`) + `,
			` + callerScenario("no-caller", `null`, `null`, `["e1","e2","e3"]`) + `,
			` + callerScenario("wrong", `{"subjectId":"s2","role":"staff"}`, `null`, `["e1","e2"]`) + `]}]}`

func callerScenario(id, caller, params, entryIDs string) string {
	when := `{"readModelId":"time-entries"`
	if caller != "null" {
		when += `,"caller":` + caller
	}
	if params != "null" {
		when += `,"queryParams":` + params
	}
	when += `}`
	staff := map[string]string{"e1": "s1", "e2": "s2", "e3": "s3"}
	project := map[string]string{"e1": "p1", "e2": "p1", "e3": "p2"}
	var rows []string
	for _, e := range strings.Split(strings.Trim(entryIDs, "[]"), ",") {
		e = strings.Trim(e, `"`)
		if e == "" {
			continue
		}
		rows = append(rows, `{"entryId":"`+e+`","staffId":"`+staff[e]+`","projectId":"`+project[e]+`"}`)
	}
	return `{"id":"` + id + `","name":"` + id + `","kind":"stateView",
		"given":[
			{"eventId":"entry-logged","data":{"entryId":"e1","staffId":"s1","projectId":"p1"}},
			{"eventId":"entry-logged","data":{"entryId":"e2","staffId":"s2","projectId":"p1"}},
			{"eventId":"entry-logged","data":{"entryId":"e3","staffId":"s3","projectId":"p2"}},
			{"eventId":"pm-assigned","data":{"assignmentId":"a1","staffId":"s9","projectId":"p1"}}],
		"when":` + when + `,
		"then":{"result":{"entries":[` + strings.Join(rows, ",") + `]}}}`
}

// A stateView scenario naming a `caller` (schema 3.8.0) reads as that
// caller: verify runs the generated read rule in a scratch PocketBase app
// (the rule that will actually be enforced; decision 9). A granting
// scope's param names the caller whatever it was given; selfAccess.param
// narrows even a manager; with no caller, the reader holds requiredRole.
func TestCallerScenariosRunTheGeneratedRule(t *testing.T) {
	doc, err := Load(writeDoc(t, callerDoc))
	if err != nil {
		t.Fatal(err)
	}
	// the subject id from a named field, and from the record id (the
	// import's default)
	for _, subjectField := range []string{"staffId", ""} {
		t.Run("subject="+subjectField, func(t *testing.T) {
			mapped, err := Map(doc, Options{SubjectField: subjectField})
			if err != nil {
				t.Fatal(err)
			}
			results, err := Verify(doc, mapped, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			byID := map[string]ScenarioResult{}
			for _, r := range results {
				byID[r.ScenarioID] = r
			}
			for _, id := range []string{"manager", "own", "pm", "pm-param-ignored", "mine", "no-caller"} {
				if r, ok := byID[id]; !ok || !r.Passed {
					t.Errorf("%s should pass: %+v", id, r)
				}
			}
			if r := byID["wrong"]; r.Passed {
				t.Errorf("a caller shown rows the rule withholds must fail: %+v", r)
			}
		})
	}
}
