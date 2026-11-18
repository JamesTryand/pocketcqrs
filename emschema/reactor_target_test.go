package emschema

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
)

// reactorDoc has three automations, each naming where its command goes
// (schema 5.0.0 targetIdField):
//   - auto-ship ships the order itself (its trigger's own stream, by orderId);
//   - notify raises a NEW notification for every comment (the comment carries
//     the id, commentId, that names it);
//   - credit-customer records an order on the customer's EXISTING stream, by
//     the order's customerId: the case no derived id could ever reach.
//
// The wrong-customer scenario sends its command to another customer than the
// trigger names, and must fail: before 5.0.0 every such scenario passed.
const reactorDoc = `{"eventModelingSchemaVersion":"5.0.0","id":"o","name":"O",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{
		"order-placed":{"name":"Order Placed","swimlaneId":"s","aggregate":"order",
			"fields":[{"name":"orderId","type":"string","idAttribute":true},{"name":"customerId","type":"string"}]},
		"comment-added":{"name":"Comment Added","swimlaneId":"s","aggregate":"order",
			"fields":[{"name":"orderId","type":"string","idAttribute":true},{"name":"commentId","type":"string"},{"name":"text","type":"string"}]},
		"order-shipped":{"name":"Order Shipped","swimlaneId":"s","aggregate":"order",
			"fields":[{"name":"orderId","type":"string","idAttribute":true}]},
		"notification-raised":{"name":"Notification Raised","swimlaneId":"s","aggregate":"notification",
			"fields":[{"name":"commentId","type":"string","idAttribute":true},{"name":"text","type":"string"}]},
		"customer-registered":{"name":"Customer Registered","swimlaneId":"s","aggregate":"customer",
			"fields":[{"name":"customerId","type":"string","idAttribute":true}]},
		"customer-ordered":{"name":"Customer Ordered","swimlaneId":"s","aggregate":"customer",
			"fields":[{"name":"customerId","type":"string","idAttribute":true},{"name":"orderId","type":"string"}]}},
	"commands":{
		"place-order":{"name":"Place Order","aggregate":"order","fields":[{"name":"orderId","type":"string","idAttribute":true},{"name":"customerId","type":"string"}]},
		"ship-order":{"name":"Ship Order","aggregate":"order","fields":[{"name":"orderId","type":"string","idAttribute":true}]},
		"add-comment":{"name":"Add Comment","aggregate":"order","fields":[{"name":"orderId","type":"string","idAttribute":true},{"name":"commentId","type":"string"},{"name":"text","type":"string"}]},
		"raise-notification":{"name":"Raise Notification","aggregate":"notification","fields":[{"name":"commentId","type":"string","idAttribute":true},{"name":"text","type":"string"}]},
		"register-customer":{"name":"Register Customer","aggregate":"customer","fields":[{"name":"customerId","type":"string","idAttribute":true}]},
		"record-customer-order":{"name":"Record Customer Order","aggregate":"customer","fields":[{"name":"customerId","type":"string","idAttribute":true},{"name":"orderId","type":"string"}]}},
	"screens":{"sc":{"name":"Sc"}},
	"automations":{"auto-ship":{"name":"Auto Ship"},"notify":{"name":"Notify"},"credit-customer":{"name":"Credit Customer"}},
	"slices":[
		{"id":"place","name":"Place","pattern":"stateChange","swimlaneId":"s","status":"planned",
		 "screenId":"sc","commandId":"place-order","eventIds":["order-placed"],"scenarios":[]},
		{"id":"comment","name":"Comment","pattern":"stateChange","swimlaneId":"s","status":"planned",
		 "screenId":"sc","commandId":"add-comment","eventIds":["comment-added"],"scenarios":[]},
		{"id":"register","name":"Register","pattern":"stateChange","swimlaneId":"s","status":"planned",
		 "screenId":"sc","commandId":"register-customer","eventIds":["customer-registered"],"scenarios":[]},
		{"id":"auto-ship","name":"Auto Ship","pattern":"automation","swimlaneId":"s","status":"planned",
		 "automationId":"auto-ship","triggerEventIds":["order-placed"],"targetIdField":"orderId",
		 "commandId":"ship-order","resultEventIds":["order-shipped"],
		 "scenarios":[{"id":"ships","name":"ships","kind":"stateChange",
			"given":[{"eventId":"order-placed","data":{"orderId":"o1","customerId":"c1"}}],
			"when":{"commandId":"ship-order","data":{"orderId":"o1"}},
			"then":{"events":[{"eventId":"order-shipped","data":{"orderId":"o1"}}]}}]},
		{"id":"notify","name":"Notify","pattern":"automation","swimlaneId":"s","status":"planned",
		 "automationId":"notify","triggerEventIds":["comment-added"],"targetIdField":"commentId",
		 "commandId":"raise-notification","resultEventIds":["notification-raised"],
		 "scenarios":[{"id":"raises","name":"raises","kind":"stateChange",
			"given":[{"eventId":"comment-added","data":{"orderId":"o1","commentId":"k1","text":"hi"}}],
			"when":{"commandId":"raise-notification","data":{"commentId":"k1","text":"hi"}},
			"then":{"events":[{"eventId":"notification-raised","data":{"commentId":"k1","text":"hi"}}]}}]},
		{"id":"credit-customer","name":"Credit Customer","pattern":"automation","swimlaneId":"s","status":"planned",
		 "automationId":"credit-customer","triggerEventIds":["order-placed"],"targetIdField":"customerId",
		 "commandId":"record-customer-order","resultEventIds":["customer-ordered"],
		 "scenarios":[
			{"id":"credits","name":"the order's customer","kind":"stateChange",
			 "given":[{"eventId":"customer-registered","data":{"customerId":"c1"}},
			          {"eventId":"order-placed","data":{"orderId":"o1","customerId":"c1"}}],
			 "when":{"commandId":"record-customer-order","data":{"customerId":"c1","orderId":"o1"}},
			 "then":{"events":[{"eventId":"customer-ordered","data":{"customerId":"c1","orderId":"o1"}}]}},
			{"id":"wrong-customer","name":"another customer","kind":"stateChange",
			 "given":[{"eventId":"customer-registered","data":{"customerId":"c2"}},
			          {"eventId":"order-placed","data":{"orderId":"o1","customerId":"c1"}}],
			 "when":{"commandId":"record-customer-order","data":{"customerId":"c2","orderId":"o1"}},
			 "then":{"events":[{"eventId":"customer-ordered","data":{"customerId":"c2","orderId":"o1"}}]}},
			{"id":"no-trigger","name":"no trigger","kind":"stateChange",
			 "given":[{"eventId":"customer-registered","data":{"customerId":"c1"}}],
			 "when":{"commandId":"record-customer-order","data":{"customerId":"c1","orderId":"o1"}},
			 "then":{"events":[{"eventId":"customer-ordered","data":{"customerId":"c1","orderId":"o1"}}]}}]}]}`

// Where a generated reactor sends its command: the stream named by the
// trigger event's targetIdField, and nothing else.
//   - auto-ship: the order's own stream, by orderId.
//   - notify: one notification per comment, by the comment's commentId (it
//     used to be a prefix plus the event's id, which no reader could see).
//   - credit-customer: the customer's existing stream, by customerId. A
//     derived id could not reach it: that was the commerce model's bug.
func TestReactorTargets(t *testing.T) {
	doc, err := Load(writeDoc(t, reactorDoc))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatal(err)
	}

	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	appendEv := func(seq int64, typ, data string) {
		if _, err := store.Append(ctx, "order", "o1", seq, []events.NewEvent{{Type: typ, Data: json.RawMessage(data)}}); err != nil {
			t.Fatal(err)
		}
	}
	appendEv(0, "OrderPlaced", `{"orderId":"o1","customerId":"c1"}`)
	appendEv(1, "CommentAdded", `{"orderId":"o1","commentId":"k1","text":"one"}`)
	appendEv(2, "CommentAdded", `{"orderId":"o1","commentId":"k2","text":"two"}`)

	run := func(name string) []functions.DispatchPreview {
		t.Helper()
		for _, d := range mapped.Domains {
			files, err := d.Generate()
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				if f.Name != name+".js" {
					continue
				}
				spec, err := functions.LoadReactorSource(functions.NewGojaRuntime(nil), f.Name, f.Source)
				if err != nil {
					t.Fatal(err)
				}
				out, err := functions.DryRunReactor(store, spec)
				if err != nil {
					t.Fatal(err)
				}
				return out.Dispatches
			}
		}
		t.Fatalf("no reactor %s was generated", name)
		return nil
	}

	if ship := run("autoShip"); len(ship) != 1 || ship[0].Aggregate != "order" || ship[0].ID != "o1" {
		t.Errorf("auto-ship must ship the order it was triggered by (order/o1): %+v", ship)
	}
	notify := run("notify")
	if len(notify) != 2 || notify[0].ID != "k1" || notify[1].ID != "k2" || notify[0].Aggregate != "notification" {
		t.Errorf("each comment raises the notification its commentId names (k1, k2): %+v", notify)
	}
	if credit := run("creditCustomer"); len(credit) != 1 || credit[0].Aggregate != "customer" || credit[0].ID != "c1" {
		t.Errorf("credit-customer must reach the order's customer, customer/c1: %+v", credit)
	}
}

// Verify runs an automation scenario on the stream its trigger names, and
// fails a scenario whose command goes anywhere else, or that shows no trigger.
func TestVerifyChecksWhereAnAutomationsCommandGoes(t *testing.T) {
	doc, err := Load(writeDoc(t, reactorDoc))
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
	for _, id := range []string{"ships", "raises", "credits"} {
		if r := byID[id]; !r.Passed {
			t.Errorf("%s: %+v", id, r)
		}
	}
	if r := byID["wrong-customer"]; r.Passed || !strings.Contains(r.Detail, `customerId "c1"`) || !strings.Contains(r.Detail, `customerId "c2"`) {
		t.Errorf("a command sent to another customer than the trigger names must fail, naming both: %+v", r)
	}
	if r := byID["no-trigger"]; r.Passed || !strings.Contains(r.Detail, "gives none of its trigger events") {
		t.Errorf("an automation scenario without its trigger must fail: %+v", r)
	}
}

// An automation that doesn't say where its command goes, or names a field the
// trigger or the command lacks, is refused at import, by name.
func TestAutomationTargetIsRequiredAndChecked(t *testing.T) {
	cases := []struct{ name, old, new, want string }{
		{"no targetIdField", `"targetIdField":"customerId",`, ``,
			`/slices/5: missing property 'targetIdField'`},
		{"not on the trigger", `"targetIdField":"customerId",`, `"targetIdField":"buyerId",`,
			`its targetIdField "buyerId" is not a field of trigger event "order-placed"`},
		{"not the command's identity",
			`"record-customer-order":{"name":"Record Customer Order","aggregate":"customer","fields":[{"name":"customerId","type":"string","idAttribute":true}`,
			`"record-customer-order":{"name":"Record Customer Order","aggregate":"customer","fields":[{"name":"customerId","type":"string"}`,
			`command "record-customer-order" must declare "customerId" with idAttribute: true`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(reactorDoc, c.old) {
				t.Fatalf("the fixture lost %q", c.old)
			}
			doc, err := Load(writeDoc(t, strings.Replace(reactorDoc, c.old, c.new, 1)))
			if err == nil {
				_, err = Map(doc, Options{})
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want %q, got %v", c.want, err)
			}
		})
	}
}
