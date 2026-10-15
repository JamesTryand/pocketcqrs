package emschema

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
)

// reactorDoc has two automations triggered from an order's stream:
//   - auto-ship ships the order itself (the same aggregate as its trigger);
//   - notify raises a NEW notification for every comment added to an order
//     (a create on another aggregate, once per trigger event: an order gets
//     many comments).
const reactorDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"o","name":"O",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{
		"order-placed":{"name":"Order Placed","swimlaneId":"s","aggregate":"order",
			"fields":[{"name":"orderId","type":"string","idAttribute":true}]},
		"comment-added":{"name":"Comment Added","swimlaneId":"s","aggregate":"order",
			"fields":[{"name":"orderId","type":"string","idAttribute":true},{"name":"text","type":"string"}]},
		"order-shipped":{"name":"Order Shipped","swimlaneId":"s","aggregate":"order",
			"fields":[{"name":"orderId","type":"string","idAttribute":true}]},
		"notification-raised":{"name":"Notification Raised","swimlaneId":"s","aggregate":"notification",
			"fields":[{"name":"text","type":"string"}]}},
	"commands":{
		"ship-order":{"name":"Ship Order","aggregate":"order","fields":[{"name":"orderId","type":"string","idAttribute":true}]},
		"add-comment":{"name":"Add Comment","aggregate":"order","fields":[{"name":"orderId","type":"string","idAttribute":true},{"name":"text","type":"string"}]},
		"raise-notification":{"name":"Raise Notification","aggregate":"notification","fields":[{"name":"text","type":"string"}]}},
	"screens":{"sc":{"name":"Sc"}},
	"automations":{"auto-ship":{"name":"Auto Ship"},"notify":{"name":"Notify"}},
	"slices":[
		{"id":"comment","name":"Comment","pattern":"stateChange","swimlaneId":"s","status":"planned",
		 "screenId":"sc","commandId":"add-comment","eventIds":["comment-added"],"scenarios":[]},
		{"id":"auto-ship","name":"Auto Ship","pattern":"automation","swimlaneId":"s","status":"planned",
		 "automationId":"auto-ship","triggerEventIds":["order-placed"],"commandId":"ship-order","resultEventIds":["order-shipped"],
		 "scenarios":[{"id":"ships","name":"ships","kind":"stateChange",
			"given":[{"eventId":"order-placed","data":{"orderId":"o1"}}],
			"when":{"commandId":"ship-order","data":{"orderId":"o1"}},
			"then":{"events":[{"eventId":"order-shipped","data":{"orderId":"o1"}}]}}]},
		{"id":"notify","name":"Notify","pattern":"automation","swimlaneId":"s","status":"planned",
		 "automationId":"notify","triggerEventIds":["comment-added"],"commandId":"raise-notification","resultEventIds":["notification-raised"],
		 "scenarios":[{"id":"raises","name":"raises","kind":"stateChange",
			"given":[{"eventId":"comment-added","data":{"orderId":"o1","text":"hi"}}],
			"when":{"commandId":"raise-notification","data":{"text":"hi"}},
			"then":{"events":[{"eventId":"notification-raised","data":{"text":"hi"}}]}}]}]}`

// R3: where a generated reactor sends its command.
//   - Same aggregate as the trigger: the trigger's own stream. It used to
//     be prefix + that stream id, a stream that never exists, so auto-ship
//     shipped nothing ("order does not exist").
//   - A create on another aggregate: one target per triggering EVENT,
//     prefix + the event's id. It used to be prefix + the source stream id,
//     so a second comment on the same order hit "already exists" and its
//     notification was never raised.
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
	appendEv := func(seq int64, typ, data string) events.Event {
		got, err := store.Append(ctx, "order", "o1", seq, []events.NewEvent{{Type: typ, Data: json.RawMessage(data)}})
		if err != nil {
			t.Fatal(err)
		}
		return got[0]
	}
	appendEv(0, "OrderPlaced", `{"orderId":"o1"}`)
	c1 := appendEv(1, "CommentAdded", `{"orderId":"o1","text":"one"}`)
	c2 := appendEv(2, "CommentAdded", `{"orderId":"o1","text":"two"}`)

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

	ship := run("autoShip")
	if len(ship) != 1 || ship[0].Aggregate != "order" || ship[0].ID != "o1" {
		t.Errorf("auto-ship must ship the order it was triggered by (order/o1): %+v", ship)
	}

	notify := run("notify")
	if len(notify) != 2 {
		t.Fatalf("want a notification per comment, got %+v", notify)
	}
	for i, want := range []events.Event{c1, c2} {
		// prefixed by the automation, so two automations reacting to the
		// same event on one target aggregate never collide
		if notify[i].Aggregate != "notification" || notify[i].ID != "notify-"+want.ID {
			t.Errorf("comment %d must raise its own notification, keyed by the comment event's id %s: %+v", i+1, want.ID, notify[i])
		}
	}
	if notify[0].ID == notify[1].ID {
		t.Errorf("two comments on one order must not share a notification id: %+v", notify)
	}
}
