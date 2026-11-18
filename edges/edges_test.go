package edges

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/jamestryand/pocketcqrs/events"
)

var out = []Out{{EventType: "ContactCaptured", System: "personal-data", Event: "contact-captured", Version: 1,
	Fields: []string{"contactId", "siteId"}}}

var in = In{ID: "contact-starts-lead", FromSystem: "personal-data", FromEvent: "contact-captured", FromVersion: 1,
	Aggregate: "lead", Command: "StartLead", TargetField: "contactId",
	Fields: map[string]string{"leadId": "contactId", "siteId": "siteId"}}

// What a sender publishes is only what some edge maps: the address stays.
func TestPublishSendsOnlyTheMappedFields(t *testing.T) {
	ev := events.Event{ID: "ev-1", Type: "ContactCaptured",
		Data: json.RawMessage(`{"contactId":"c-1","siteId":"hugo-demo","email":"a@example.org","name":"A"}`)}
	m, ok, err := Publish(out, ev)
	if err != nil || !ok {
		t.Fatalf("not published: %v", err)
	}
	if _, leaked := m.Data["email"]; leaked || len(m.Data) != 2 {
		t.Fatalf("published %v: only contactId and siteId may leave", m.Data)
	}
	if m.ID != "ev-1" || m.Event != "contact-captured" || m.Version != 1 || m.CorrelationID != "ev-1" {
		t.Errorf("the message is %+v", m)
	}
	if _, ok, _ := Publish(out, events.Event{Type: "ContactConfirmed", Data: json.RawMessage(`{}`)}); ok {
		t.Error("an event no edge starts from was published")
	}
}

// The receiver's command goes to the stream the target field names, with the
// mapped fields.
func TestTranslateMakesTheCommand(t *testing.T) {
	m := Message{ID: "ev-1", System: "personal-data", Event: "contact-captured", Version: 1,
		Data: map[string]any{"contactId": "c-1", "siteId": "hugo-demo"}}
	if got := Find([]In{in}, m); len(got) != 1 {
		t.Fatalf("the edge does not match its own event: %v", got)
	}
	agg, id, cmd, err := in.Translate(m)
	if err != nil {
		t.Fatal(err)
	}
	if agg != "lead" || id != "c-1" || cmd.Name != "StartLead" || string(cmd.Payload) != `{"leadId":"c-1","siteId":"hugo-demo"}` {
		t.Errorf("got %s %s %s %s", agg, id, cmd.Name, cmd.Payload)
	}
	m.Version = 2
	if len(Find([]In{in}, m)) != 0 {
		t.Error("a version the edge does not name was matched")
	}
}

func TestNoTargetIsDropped(t *testing.T) {
	_, _, _, err := in.Translate(Message{Data: map[string]any{"siteId": "x", "contactId": ""}})
	if !errors.Is(err, ErrNoTarget) {
		t.Fatalf("want ErrNoTarget, got %v", err)
	}
}
