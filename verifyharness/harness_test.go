package verifyharness

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/kms"
	"github.com/jamestryand/pocketcqrs/pii"
	"github.com/jamestryand/pocketcqrs/verifyharness/wire"
)

// envelopeProjection stores an event's note as a generated Go projection
// stores personal data: the envelope's JSON text.
type envelopeProjection struct{ app core.App }

func (p envelopeProjection) Apply(_ context.Context, ev events.Event) error {
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		return err
	}
	col, err := p.app.FindCollectionByNameOrId("notes")
	if err != nil {
		return err
	}
	rec := core.NewRecord(col)
	rec.Set("noteId", data["noteId"])
	env, _ := json.Marshal(data["note"])
	rec.Set("note", string(env))
	return p.app.Save(rec)
}

// A sealed value in a row is revealed before it is compared: the model's
// values are plain.
func TestRowsAreRevealed(t *testing.T) {
	ctx := context.Background()
	keys := kms.NewMemory()
	if err := keys.EnsureKey(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	cipher, err := keys.Encrypt(ctx, "s1", []byte(`"call Ada"`))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"noteId": "n1", "note": map[string]any{"$pii": map[string]any{"s": "s1", "c": cipher}}})
	spec := Spec{
		Register:    func(*decider.Registry, pii.Deps) {},
		Projections: map[string]func(core.App) Projection{"notes": func(app core.App) Projection { return envelopeProjection{app} }},
	}
	out := answer(spec, wire.Input{
		Collections: []wire.Collection{{Name: "notes", Key: "noteId", Fields: []wire.Field{{Name: "noteId", Type: "text"}, {Name: "note", Type: "text", PII: true}}}},
		Views:       []wire.View{{ID: "v", Runs: []wire.Run{{Collection: "notes", Events: []events.Event{{Position: 1, Type: "Noted", Data: data}}}}}},
	}, keys)
	if out.Views[0].Error != "" {
		t.Fatal(out.Views[0].Error)
	}
	if got := out.Views[0].Rows["notes"]["n1"]["note"]; got != "call Ada" {
		t.Fatalf("note = %#v, want the plaintext", got)
	}
}
