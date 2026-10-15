package emschema

import (
	"strings"
	"testing"
)

// widgetErrorDoc has one create command and two error scenarios that create
// a widget that already exists: one names the generated decider's refusal,
// the other a refusal the decider never gives.
func widgetErrorDoc(t *testing.T) *Document {
	t.Helper()
	p := writeDoc(t, `{"eventModelingSchemaVersion":"3.17.0","id":"w","name":"W",
		"swimlanes":[{"id":"s","name":"S","kind":"team"}],
		"events":{"widget-created":{"name":"Widget Created","swimlaneId":"s","aggregate":"widget",
			"fields":[{"name":"widgetId","type":"string","idAttribute":true}]}},
		"commands":{"create-widget":{"name":"Create Widget","aggregate":"widget",
			"fields":[{"name":"widgetId","type":"string","idAttribute":true}]}},
		"screens":{"sc":{"name":"Sc"}},
		"slices":[{"id":"create","name":"Create","pattern":"stateChange","swimlaneId":"s","status":"planned",
			"screenId":"sc","commandId":"create-widget","eventIds":["widget-created"],"scenarios":[
			{"id":"create","name":"a new widget","kind":"stateChange","given":[],
			 "when":{"commandId":"create-widget","data":{"widgetId":"w1"}},
			 "then":{"events":[{"eventId":"widget-created","data":{"widgetId":"w1"}}]}},
			{"id":"its-own-refusal","name":"its own refusal","kind":"error",
			 "given":[{"eventId":"widget-created","data":{"widgetId":"w1"}}],
			 "when":{"commandId":"create-widget","data":{"widgetId":"w1"},"actor":"u1","at":"2026-10-10T09:00:00Z"},
			 "then":{"error":{"message":"widget already exists"}}},
			{"id":"another-refusal","name":"another refusal","kind":"error",
			 "given":[{"eventId":"widget-created","data":{"widgetId":"w1"}}],
			 "when":{"commandId":"create-widget","data":{"widgetId":"w1"}},
			 "then":{"error":{"message":"widgets are frozen on Fridays"}}}]}]}`)
	doc, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// T2: an error scenario passes only on its own refusal. Any refusal used to
// pass, so a scenario naming a rule the decider doesn't have (a refusal the
// model declares and the code lacks) reported PASS.
func TestErrorScenarioPassesOnlyOnItsOwnMessage(t *testing.T) {
	doc := widgetErrorDoc(t)
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
	if r := byID["its-own-refusal"]; !r.Passed {
		t.Errorf("refused with the scenario's own message, so it passes: %+v", r)
	}
	r := byID["another-refusal"]
	if r.Passed {
		t.Fatalf("refused for another reason, so it fails: %+v", r)
	}
	for _, want := range []string{"widgets are frozen on Fridays", "widget already exists"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("the failure should name both messages; missing %q: %s", want, r.Detail)
		}
	}
}

// T2: a scenario's when.actor and when.at (schema 3.10.0) are who and when
// the command runs as, so the decider sees them as command.actor and
// command.now. Without them the verifier's defaults stand.
func TestScenarioActorAndTimeReachTheDecider(t *testing.T) {
	meta := commandMeta(CommandRef{Actor: "u1", At: "2026-10-10T09:00:00Z"})
	if meta["actor"] != "u1" || meta["now"] != "2026-10-10T09:00:00Z" {
		t.Fatalf("the scenario's actor and time were not passed: %v", meta)
	}
	meta = commandMeta(CommandRef{})
	if meta["actor"] != defaultScenarioActor || meta["now"] != defaultScenarioNow {
		t.Fatalf("a scenario naming neither gets the defaults: %v", meta)
	}
}
