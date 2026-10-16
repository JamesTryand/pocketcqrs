package emschema

import (
	"strings"
	"testing"
)

// endedDoc is a post that is published once and deleted for good: its
// delete ends the stream, and no scenario shows a publish after a delete, so
// the end is final. A deleted post's id can never be published again.
const endedDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"e","name":"E",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{
		"post-published":{"name":"Post Published","swimlaneId":"s","aggregate":"post",
			"fields":[{"name":"postId","type":"string","idAttribute":true}]},
		"post-deleted":{"name":"Post Deleted","swimlaneId":"s","aggregate":"post","endsStream":true,
			"fields":[{"name":"postId","type":"string","idAttribute":true}]}},
	"commands":{
		"publish-post":{"name":"Publish Post","aggregate":"post","fields":[{"name":"postId","type":"string","idAttribute":true}]},
		"delete-post":{"name":"Delete Post","aggregate":"post","fields":[{"name":"postId","type":"string","idAttribute":true}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"publish","name":"Publish","pattern":"stateChange","swimlaneId":"s","status":"planned",
		 "screenId":"sc","commandId":"publish-post","eventIds":["post-published"],"scenarios":[
			{"id":"publish-new","name":"a new post","kind":"stateChange","given":[],
			 "when":{"commandId":"publish-post","data":{"postId":"p1"}},
			 "then":{"events":[{"eventId":"post-published","data":{"postId":"p1"}}]}},
			{"id":"publish-deleted","name":"a deleted post is not published again","kind":"error",
			 "given":[{"eventId":"post-published","data":{"postId":"p1"}},{"eventId":"post-deleted","data":{"postId":"p1"}}],
			 "when":{"commandId":"publish-post","data":{"postId":"p1"}},
			 "then":{"error":{"message":"post has ended and cannot begin again"}}}]},
		{"id":"delete","name":"Delete","pattern":"stateChange","swimlaneId":"s","status":"planned",
		 "screenId":"sc","commandId":"delete-post","eventIds":["post-deleted"],"scenarios":[
			{"id":"delete-it","name":"delete a post","kind":"stateChange",
			 "given":[{"eventId":"post-published","data":{"postId":"p1"}}],
			 "when":{"commandId":"delete-post","data":{"postId":"p1"}},
			 "then":{"events":[{"eventId":"post-deleted","data":{"postId":"p1"}}]}}]}]}`

// reassignDoc is the other case: a team lead is assigned, unassigned and
// assigned again. Its assign slice has a scenario doing exactly that, so
// the create may begin the stream again (an endsStream event still resets
// exists).
const reassignDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"r","name":"R",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{
		"lead-assigned":{"name":"Lead Assigned","swimlaneId":"s","aggregate":"assignment",
			"fields":[{"name":"assignmentId","type":"string","idAttribute":true}]},
		"lead-unassigned":{"name":"Lead Unassigned","swimlaneId":"s","aggregate":"assignment","endsStream":true,
			"fields":[{"name":"assignmentId","type":"string","idAttribute":true}]}},
	"commands":{
		"assign-lead":{"name":"Assign Lead","aggregate":"assignment","fields":[{"name":"assignmentId","type":"string","idAttribute":true}]},
		"unassign-lead":{"name":"Unassign Lead","aggregate":"assignment","fields":[{"name":"assignmentId","type":"string","idAttribute":true}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"assign","name":"Assign","pattern":"stateChange","swimlaneId":"s","status":"planned",
		 "screenId":"sc","commandId":"assign-lead","eventIds":["lead-assigned"],"scenarios":[
			{"id":"assign-new","name":"assign","kind":"stateChange","given":[],
			 "when":{"commandId":"assign-lead","data":{"assignmentId":"a1"}},
			 "then":{"events":[{"eventId":"lead-assigned","data":{"assignmentId":"a1"}}]}},
			{"id":"assign-again","name":"assign again after an unassign","kind":"stateChange",
			 "given":[{"eventId":"lead-assigned","data":{"assignmentId":"a1"}},{"eventId":"lead-unassigned","data":{"assignmentId":"a1"}}],
			 "when":{"commandId":"assign-lead","data":{"assignmentId":"a1"}},
			 "then":{"events":[{"eventId":"lead-assigned","data":{"assignmentId":"a1"}}]}},
			{"id":"assign-twice","name":"an assigned lead is not assigned twice","kind":"error",
			 "given":[{"eventId":"lead-assigned","data":{"assignmentId":"a1"}}],
			 "when":{"commandId":"assign-lead","data":{"assignmentId":"a1"}},
			 "then":{"error":{"message":"assignment already exists"}}}]},
		{"id":"unassign","name":"Unassign","pattern":"stateChange","swimlaneId":"s","status":"planned",
		 "screenId":"sc","commandId":"unassign-lead","eventIds":["lead-unassigned"],"scenarios":[
			{"id":"unassign-it","name":"unassign","kind":"stateChange",
			 "given":[{"eventId":"lead-assigned","data":{"assignmentId":"a1"}}],
			 "when":{"commandId":"unassign-lead","data":{"assignmentId":"a1"}},
			 "then":{"events":[{"eventId":"lead-unassigned","data":{"assignmentId":"a1"}}]}}]}]}`

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

// A create must not restart a stream that ended for good. endsStream
// reset `exists` to false, so the create's own "already exists" check
// passed again and a deleted post could be published a second time.
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
