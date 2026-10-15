package emschema

import (
	"strings"
	"testing"
)

// endedDoc is an entry that is logged once and deleted for good (D28): its
// delete ends the stream, and no scenario shows a log after a delete, so the
// end is final. A deleted entry's id can never be logged again.
const endedDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"e","name":"E",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{
		"entry-logged":{"name":"Entry Logged","swimlaneId":"s","aggregate":"entry",
			"fields":[{"name":"entryId","type":"string","idAttribute":true}]},
		"entry-deleted":{"name":"Entry Deleted","swimlaneId":"s","aggregate":"entry","endsStream":true,
			"fields":[{"name":"entryId","type":"string","idAttribute":true}]}},
	"commands":{
		"log-entry":{"name":"Log Entry","aggregate":"entry","fields":[{"name":"entryId","type":"string","idAttribute":true}]},
		"delete-entry":{"name":"Delete Entry","aggregate":"entry","fields":[{"name":"entryId","type":"string","idAttribute":true}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"log","name":"Log","pattern":"stateChange","swimlaneId":"s","status":"planned",
		 "screenId":"sc","commandId":"log-entry","eventIds":["entry-logged"],"scenarios":[
			{"id":"log-new","name":"a new entry","kind":"stateChange","given":[],
			 "when":{"commandId":"log-entry","data":{"entryId":"e1"}},
			 "then":{"events":[{"eventId":"entry-logged","data":{"entryId":"e1"}}]}},
			{"id":"log-deleted","name":"a deleted entry is not logged again","kind":"error",
			 "given":[{"eventId":"entry-logged","data":{"entryId":"e1"}},{"eventId":"entry-deleted","data":{"entryId":"e1"}}],
			 "when":{"commandId":"log-entry","data":{"entryId":"e1"}},
			 "then":{"error":{"message":"entry has ended and cannot begin again"}}}]},
		{"id":"delete","name":"Delete","pattern":"stateChange","swimlaneId":"s","status":"planned",
		 "screenId":"sc","commandId":"delete-entry","eventIds":["entry-deleted"],"scenarios":[
			{"id":"delete-it","name":"delete an entry","kind":"stateChange",
			 "given":[{"eventId":"entry-logged","data":{"entryId":"e1"}}],
			 "when":{"commandId":"delete-entry","data":{"entryId":"e1"}},
			 "then":{"events":[{"eventId":"entry-deleted","data":{"entryId":"e1"}}]}}]}]}`

// reassignDoc is the other case: a project manager is assigned, unassigned
// and assigned again. Its assign slice has a scenario doing exactly that, so
// the create may begin the stream again (Finding 3 keeps working).
const reassignDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"r","name":"R",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{
		"pm-assigned":{"name":"Pm Assigned","swimlaneId":"s","aggregate":"assignment",
			"fields":[{"name":"assignmentId","type":"string","idAttribute":true}]},
		"pm-unassigned":{"name":"Pm Unassigned","swimlaneId":"s","aggregate":"assignment","endsStream":true,
			"fields":[{"name":"assignmentId","type":"string","idAttribute":true}]}},
	"commands":{
		"assign-pm":{"name":"Assign Pm","aggregate":"assignment","fields":[{"name":"assignmentId","type":"string","idAttribute":true}]},
		"unassign-pm":{"name":"Unassign Pm","aggregate":"assignment","fields":[{"name":"assignmentId","type":"string","idAttribute":true}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"assign","name":"Assign","pattern":"stateChange","swimlaneId":"s","status":"planned",
		 "screenId":"sc","commandId":"assign-pm","eventIds":["pm-assigned"],"scenarios":[
			{"id":"assign-new","name":"assign","kind":"stateChange","given":[],
			 "when":{"commandId":"assign-pm","data":{"assignmentId":"a1"}},
			 "then":{"events":[{"eventId":"pm-assigned","data":{"assignmentId":"a1"}}]}},
			{"id":"assign-again","name":"assign again after an unassign","kind":"stateChange",
			 "given":[{"eventId":"pm-assigned","data":{"assignmentId":"a1"}},{"eventId":"pm-unassigned","data":{"assignmentId":"a1"}}],
			 "when":{"commandId":"assign-pm","data":{"assignmentId":"a1"}},
			 "then":{"events":[{"eventId":"pm-assigned","data":{"assignmentId":"a1"}}]}},
			{"id":"assign-twice","name":"an assigned pm is not assigned twice","kind":"error",
			 "given":[{"eventId":"pm-assigned","data":{"assignmentId":"a1"}}],
			 "when":{"commandId":"assign-pm","data":{"assignmentId":"a1"}},
			 "then":{"error":{"message":"assignment already exists"}}}]},
		{"id":"unassign","name":"Unassign","pattern":"stateChange","swimlaneId":"s","status":"planned",
		 "screenId":"sc","commandId":"unassign-pm","eventIds":["pm-unassigned"],"scenarios":[
			{"id":"unassign-it","name":"unassign","kind":"stateChange",
			 "given":[{"eventId":"pm-assigned","data":{"assignmentId":"a1"}}],
			 "when":{"commandId":"unassign-pm","data":{"assignmentId":"a1"}},
			 "then":{"events":[{"eventId":"pm-unassigned","data":{"assignmentId":"a1"}}]}}]}]}`

func verifyDoc(t *testing.T, body string) map[string]ScenarioResult {
	t.Helper()
	doc, err := Load(writeDoc(t, body))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{})
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
	return byID
}

// D28: a create must not restart a stream that ended for good. endsStream
// reset `exists` to false, so the create's own "already exists" check
// passed again and a deleted entry could be logged a second time.
func TestEndedStreamIsNotBegunAgain(t *testing.T) {
	results := verifyDoc(t, endedDoc)
	for id, r := range results {
		if !r.Passed {
			t.Errorf("%s: %s", id, r.Detail)
		}
	}
	if len(results) != 3 {
		t.Fatalf("want 3 scenarios, got %d", len(results))
	}
}

// ...and a create that a scenario shows beginning the stream again after
// the end still may (an assign after an unassign).
func TestStreamWithBeginAgainEvidenceMayBeginAgain(t *testing.T) {
	results := verifyDoc(t, reassignDoc)
	for id, r := range results {
		if !r.Passed {
			t.Errorf("%s: %s", id, r.Detail)
		}
	}
	if len(results) != 4 {
		t.Fatalf("want 4 scenarios, got %d", len(results))
	}
}

// An aggregate with no final end generates exactly as before: no `ended`
// flag in its state.
func TestNoEndedFlagWithoutAFinalEnd(t *testing.T) {
	doc, err := Load(writeDoc(t, reassignDoc))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range mapped.Domains {
		files, err := d.Generate()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(files[0].Source, "ended") {
			t.Errorf("%s has no final end, so no ended flag:\n%s", d.Aggregate, files[0].Source)
		}
	}
}
