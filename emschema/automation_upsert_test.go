package emschema

import "testing"

// Two automations each record on one ledger (time and expense entries
// landing on the payroll): each slice shows the ledger both fresh and
// already there, so each command is an upsert, not the create. dotnetcqrs
// maps an automation's evidence this way (MapAutomation); without it both
// were creates and the import stopped with "a stream has one beginning".
const automationUpsertDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"u","name":"U",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{
		"time-logged":{"name":"Time Logged","swimlaneId":"s","aggregate":"entry","fields":[{"name":"entryId","type":"string","idAttribute":true},{"name":"amount","type":"decimal"}]},
		"cost-logged":{"name":"Cost Logged","swimlaneId":"s","aggregate":"entry","fields":[{"name":"entryId","type":"string","idAttribute":true},{"name":"amount","type":"decimal"}]},
		"pay-recorded":{"name":"Pay Recorded","swimlaneId":"s","aggregate":"ledger","fields":[{"name":"ledgerId","type":"string","idAttribute":true},{"name":"entryId","type":"string"},{"name":"amount","type":"decimal"}]},
		"cost-recorded":{"name":"Cost Recorded","swimlaneId":"s","aggregate":"ledger","fields":[{"name":"ledgerId","type":"string","idAttribute":true},{"name":"entryId","type":"string"},{"name":"amount","type":"decimal"}]}},
	"commands":{
		"log-time":{"name":"Log Time","aggregate":"entry","fields":[{"name":"entryId","type":"string","idAttribute":true},{"name":"amount","type":"decimal"}]},
		"log-cost":{"name":"Log Cost","aggregate":"entry","fields":[{"name":"entryId","type":"string","idAttribute":true},{"name":"amount","type":"decimal"}]},
		"record-pay":{"name":"Record Pay","aggregate":"ledger","fields":[{"name":"entryId","type":"string"},{"name":"amount","type":"decimal"}]},
		"record-cost":{"name":"Record Cost","aggregate":"ledger","fields":[{"name":"entryId","type":"string"},{"name":"amount","type":"decimal"}]}},
	"automations":{"on-time":{"name":"On Time"},"on-cost":{"name":"On Cost"}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"lt","name":"Log Time","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"log-time","eventIds":["time-logged"],"scenarios":[]},
		{"id":"lc","name":"Log Cost","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"log-cost","eventIds":["cost-logged"],"scenarios":[]},
		{"id":"rp","name":"Record Pay","pattern":"automation","swimlaneId":"s","status":"planned","automationId":"on-time",
		 "triggerEventIds":["time-logged"],"commandId":"record-pay","resultEventIds":["pay-recorded"],"scenarios":[
			{"id":"first","name":"first","kind":"stateChange","given":[{"eventId":"time-logged","data":{"entryId":"e1","amount":2}}],
			 "when":{"commandId":"record-pay","data":{"entryId":"e1","amount":2}},
			 "then":{"events":[{"eventId":"pay-recorded","data":{"ledgerId":"ledger","entryId":"e1","amount":2}}]}},
			{"id":"again","name":"again","kind":"stateChange","given":[{"eventId":"pay-recorded","data":{"ledgerId":"ledger","entryId":"e1","amount":2}}],
			 "when":{"commandId":"record-pay","data":{"entryId":"e1","amount":3}},
			 "then":{"events":[{"eventId":"pay-recorded","data":{"ledgerId":"ledger","entryId":"e1","amount":3}}]}}]},
		{"id":"rc","name":"Record Cost","pattern":"automation","swimlaneId":"s","status":"planned","automationId":"on-cost",
		 "triggerEventIds":["cost-logged"],"commandId":"record-cost","resultEventIds":["cost-recorded"],"scenarios":[
			{"id":"first-cost","name":"first cost","kind":"stateChange","given":[{"eventId":"cost-logged","data":{"entryId":"x1","amount":5}}],
			 "when":{"commandId":"record-cost","data":{"entryId":"x1","amount":5}},
			 "then":{"events":[{"eventId":"cost-recorded","data":{"ledgerId":"ledger","entryId":"x1","amount":5}}]}},
			{"id":"cost-again","name":"cost again","kind":"stateChange","given":[{"eventId":"cost-recorded","data":{"ledgerId":"ledger","entryId":"x1","amount":5}}],
			 "when":{"commandId":"record-cost","data":{"entryId":"x1","amount":6}},
			 "then":{"events":[{"eventId":"cost-recorded","data":{"ledgerId":"ledger","entryId":"x1","amount":6}}]}}]}]}`

func TestAutomationWithBothEvidenceIsAnUpsert(t *testing.T) {
	doc, err := Load(writeDoc(t, automationUpsertDoc))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, d := range mapped.Domains {
		for _, c := range d.Commands {
			if c.Name == "RecordPay" || c.Name == "RecordCost" {
				found++
				if c.Once || c.RequiresExisting {
					t.Errorf("%s: once=%v requiresExisting=%v, want an upsert (neither)", c.Name, c.Once, c.RequiresExisting)
				}
			}
		}
		for _, r := range d.Reactors {
			if r.PerEvent {
				t.Errorf("reactor %s creates a stream per event; an upsert targets one", r.Name)
			}
		}
	}
	if found != 2 {
		t.Fatalf("want both ledger commands, found %d", found)
	}
}
