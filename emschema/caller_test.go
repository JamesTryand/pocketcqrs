package emschema

import (
	"strings"
	"testing"
)

// callerDoc: tickets (supervisors read all, agents their own, a team lead
// also those of their teams) and the team-lead assignments the granting
// scope reads through. Its view scenarios each
// read as a different caller; "wrong" expects what the rule does NOT
// allow, so it must fail.
var callerDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"c","name":"C",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{
		"ticket-opened":{"name":"Ticket Opened","swimlaneId":"s","aggregate":"ticket",
			"fields":[{"name":"ticketId","type":"string","idAttribute":true},{"name":"agentId","type":"string"},{"name":"teamId","type":"string"}]},
		"lead-assigned":{"name":"Lead Assigned","swimlaneId":"s","aggregate":"assignment",
			"fields":[{"name":"assignmentId","type":"string","idAttribute":true},{"name":"agentId","type":"string"},{"name":"teamId","type":"string"}]}},
	"commands":{
		"open-ticket":{"name":"Open Ticket","aggregate":"ticket","fields":[{"name":"ticketId","type":"string","idAttribute":true}]},
		"assign-lead":{"name":"Assign Lead","aggregate":"assignment","fields":[{"name":"assignmentId","type":"string","idAttribute":true}]}},
	"readModels":{
		"tickets":{"name":"Tickets","builtFromEventIds":["ticket-opened"],"requiredRole":"supervisor",
			"fields":[{"name":"ticketId","type":"string","idAttribute":true},{"name":"agentId","type":"string"},{"name":"teamId","type":"string"}],
			"selfAccess":{"subjectField":"agentId","param":"mine"},
			"scopes":[{"param":"leadAgentId","grantsAccess":true,
				"via":{"readModelId":"team-leads","matchParamTo":"agentId","selectField":"teamId","filterLocalField":"teamId"}}]},
		"team-leads":{"name":"Team Leads","builtFromEventIds":["lead-assigned"],"requiredRole":"supervisor",
			"fields":[{"name":"assignmentId","type":"string","idAttribute":true},{"name":"agentId","type":"string"},{"name":"teamId","type":"string"}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"log","name":"Log","pattern":"stateChange","swimlaneId":"s","status":"planned",
		 "screenId":"sc","commandId":"open-ticket","eventIds":["ticket-opened"],"scenarios":[]},
		{"id":"assign","name":"Assign","pattern":"stateChange","swimlaneId":"s","status":"planned",
		 "screenId":"sc","commandId":"assign-lead","eventIds":["lead-assigned"],"scenarios":[]},
		{"id":"view","name":"View","pattern":"stateView","swimlaneId":"s","status":"planned",
		 "screenId":"sc","readModelId":"tickets","scenarios":[
			` + callerScenario("supervisor", `{"subjectId":"sup1","role":"supervisor"}`, `null`, `["t1","t2","t3"]`) + `,
			` + callerScenario("own", `{"subjectId":"a2","role":"agent"}`, `null`, `["t2"]`) + `,
			` + callerScenario("lead", `{"subjectId":"a9","role":"agent"}`, `null`, `["t1","t2"]`) + `,
			` + callerScenario("lead-param-ignored", `{"subjectId":"a9","role":"agent"}`, `{"leadAgentId":"a1"}`, `["t1","t2"]`) + `,
			` + callerScenario("mine", `{"subjectId":"sup1","role":"supervisor"}`, `{"mine":true}`, `[]`) + `,
			` + callerScenario("no-caller", `null`, `null`, `["t1","t2","t3"]`) + `,
			` + callerScenario("wrong", `{"subjectId":"a2","role":"agent"}`, `null`, `["t1","t2"]`) + `]}]}`

func callerScenario(id, caller, params, ticketIDs string) string {
	when := `{"readModelId":"tickets"`
	if caller != "null" {
		when += `,"caller":` + caller
	}
	if params != "null" {
		when += `,"queryParams":` + params
	}
	when += `}`
	agent := map[string]string{"t1": "a1", "t2": "a2", "t3": "a3"}
	team := map[string]string{"t1": "g1", "t2": "g1", "t3": "g2"}
	var rows []string
	for _, e := range strings.Split(strings.Trim(ticketIDs, "[]"), ",") {
		e = strings.Trim(e, `"`)
		if e == "" {
			continue
		}
		rows = append(rows, `{"ticketId":"`+e+`","agentId":"`+agent[e]+`","teamId":"`+team[e]+`"}`)
	}
	return `{"id":"` + id + `","name":"` + id + `","kind":"stateView",
		"given":[
			{"eventId":"ticket-opened","data":{"ticketId":"t1","agentId":"a1","teamId":"g1"}},
			{"eventId":"ticket-opened","data":{"ticketId":"t2","agentId":"a2","teamId":"g1"}},
			{"eventId":"ticket-opened","data":{"ticketId":"t3","agentId":"a3","teamId":"g2"}},
			{"eventId":"lead-assigned","data":{"assignmentId":"a1","agentId":"a9","teamId":"g1"}}],
		"when":` + when + `,
		"then":{"result":{"entries":[` + strings.Join(rows, ",") + `]}}}`
}

// A stateView scenario naming a `caller` (schema 3.8.0) reads as that
// caller: verify runs the generated read rule in a scratch PocketBase app
// (the rule that will actually be enforced). A granting
// scope's param names the caller whatever it was given; selfAccess.param
// narrows even a supervisor; with no caller, the reader holds requiredRole.
func TestCallerScenariosRunTheGeneratedRule(t *testing.T) {
	doc, err := Load(writeDoc(t, callerDoc))
	if err != nil {
		t.Fatal(err)
	}
	// the subject id from a named field, and from the record id (the
	// import's default)
	for _, subjectField := range []string{"agentId", ""} {
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
			for _, id := range []string{"supervisor", "own", "lead", "lead-param-ignored", "mine", "no-caller"} {
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
