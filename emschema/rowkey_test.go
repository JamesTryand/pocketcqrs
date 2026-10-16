package emschema

import (
	"strings"
	"testing"
)

// grantRoundDoc: one grant-round stream records an award per recipient; the
// awards read model has a row per award (rowKeyField awardId, schema
// 3.14.0), and a reversal removes that award's row only.
const grantRoundDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"p","name":"P",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{
		"grant-awarded":{"name":"Grant Awarded","swimlaneId":"s","aggregate":"grantRound",
			"fields":[{"name":"grantRoundId","type":"string","idAttribute":true},{"name":"awardId","type":"string"},{"name":"recipientId","type":"string"},{"name":"amount","type":"decimal"}]},
		"award-reversed":{"name":"Award Reversed","swimlaneId":"s","aggregate":"grantRound",
			"fields":[{"name":"grantRoundId","type":"string","idAttribute":true},{"name":"awardId","type":"string"}]}},
	"commands":{
		"award":{"name":"Award","aggregate":"grantRound","fields":[{"name":"grantRoundId","type":"string","idAttribute":true},{"name":"awardId","type":"string"},{"name":"recipientId","type":"string"},{"name":"amount","type":"decimal"}]},
		"reverse":{"name":"Reverse","aggregate":"grantRound","fields":[{"name":"grantRoundId","type":"string","idAttribute":true},{"name":"awardId","type":"string"}]}},
	"readModels":{"awards":{"name":"Awards","builtFromEventIds":["grant-awarded","award-reversed"],
		"removedByEventIds":["award-reversed"],"rowKeyField":"awardId",
		"fields":[{"name":"awardId","type":"string"},{"name":"recipientId","type":"string"},{"name":"amount","type":"decimal"}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"award","name":"Award","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"award","eventIds":["grant-awarded"],"scenarios":[]},
		{"id":"rev","name":"Reverse","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"reverse","eventIds":["award-reversed"],"scenarios":[]},
		{"id":"view","name":"View","pattern":"stateView","swimlaneId":"s","status":"planned","screenId":"sc","readModelId":"awards","scenarios":[
			{"id":"two-awards","name":"two awards in one round are two rows","kind":"stateView",
			 "given":[{"eventId":"grant-awarded","data":{"grantRoundId":"r1","awardId":"award-1","recipientId":"ann","amount":100}},
			          {"eventId":"grant-awarded","data":{"grantRoundId":"r1","awardId":"award-2","recipientId":"bob","amount":200}}],
			 "when":{"readModelId":"awards"},
			 "then":{"result":{"awards":[{"awardId":"award-1","recipientId":"ann","amount":100},{"awardId":"award-2","recipientId":"bob","amount":200}]}}},
			{"id":"one-reversed","name":"a reversal removes that payment's row only","kind":"stateView",
			 "given":[{"eventId":"grant-awarded","data":{"grantRoundId":"r1","awardId":"award-1","recipientId":"ann","amount":100}},
			          {"eventId":"grant-awarded","data":{"grantRoundId":"r1","awardId":"award-2","recipientId":"bob","amount":200}},
			          {"eventId":"award-reversed","data":{"grantRoundId":"r1","awardId":"award-1"}}],
			 "when":{"readModelId":"awards"},
			 "then":{"result":{"awards":[{"awardId":"award-2","recipientId":"bob","amount":200}]}}}]}]}`

// A read model's rowKeyField (schema 3.14.0) keys each row by a field of the
// event, not its stream: one stream's events can update many rows.
func TestRowKeyFieldKeysRowsByTheEvent(t *testing.T) {
	results := verifyDoc(t, grantRoundDoc)
	for id, r := range results {
		if !r.Passed {
			t.Errorf("%s: %s", id, r.Detail)
		}
	}
	if len(results) != 2 {
		t.Fatalf("want 2 scenarios, got %d", len(results))
	}
}

// Every event a read model is built from (or removed by) must carry its
// rowKeyField, or the import is refused: such an event could name no row.
func TestRowKeyFieldMustBeOnEveryEvent(t *testing.T) {
	doc, err := Load(writeDoc(t, strings.Replace(grantRoundDoc,
		`"award-reversed":{"name":"Award Reversed","swimlaneId":"s","aggregate":"grantRound",
			"fields":[{"name":"grantRoundId","type":"string","idAttribute":true},{"name":"awardId","type":"string"}]}`,
		`"award-reversed":{"name":"Award Reversed","swimlaneId":"s","aggregate":"grantRound",
			"fields":[{"name":"grantRoundId","type":"string","idAttribute":true}]}`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Map(doc, Options{}); err == nil || !strings.Contains(err.Error(), "rowKeyField") {
		t.Fatalf("want a refusal naming rowKeyField, got %v", err)
	}
}
