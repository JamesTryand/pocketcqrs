package functions

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/scaffold"
)

// A JS decider the generator writes refuses with the exact words the gateway
// recognizes, so its two refusals carry reason does-not-exist and
// already-exists (command gateway contract v0.1, G5) through the JS runtime.
func TestGeneratedJSRefusalsCarryTheirReason(t *testing.T) {
	d := scaffold.Domain{
		Aggregate: "customerContact",
		Commands: []scaffold.Command{
			{Name: "RecordCustomerContact", Once: true, Events: []scaffold.Event{{Name: "CustomerContactRecorded", NoFields: true}}},
			{Name: "ChangeCustomerEmail", RequiresExisting: true, Events: []scaffold.Event{{Name: "CustomerEmailChanged", NoFields: true}}},
		},
	}
	files, err := d.Generate()
	if err != nil {
		t.Fatal(err)
	}
	var src string
	for _, f := range files {
		if f.Kind == "decider" {
			src = f.Source
		}
	}
	if src == "" {
		t.Fatalf("no decider generated: %+v", files)
	}

	_, registry := deciderSetup(t)
	rt := NewGojaRuntime(nil)
	spec := mkDeciderSpec(t, rt, "customerContact", src, []string{"CustomerContactRecorded", "CustomerEmailChanged"}, nil)
	registry.RegisterUntyped(spec.Aggregate, spec.UntypedDecider())
	ctx := context.Background()
	send := func(name string) error {
		_, err := registry.HandleWithMeta(ctx, "customerContact", "c1", decider.Command{Name: name, Payload: json.RawMessage(`{}`)}, nil)
		return err
	}

	err = send("ChangeCustomerEmail")
	if !decider.IsRejection(err) || err.Error() != "customer contact does not exist" {
		t.Fatalf("want the generated refusal verbatim, got %v", err)
	}
	if r := decider.RefusalReason(err, "customerContact"); r != decider.ReasonDoesNotExist {
		t.Fatalf("want reason %s, got %q", decider.ReasonDoesNotExist, r)
	}
	if err := send("RecordCustomerContact"); err != nil {
		t.Fatal(err)
	}
	err = send("RecordCustomerContact")
	if !decider.IsRejection(err) || err.Error() != "customer contact already exists" {
		t.Fatalf("want the generated refusal verbatim, got %v", err)
	}
	if r := decider.RefusalReason(err, "customerContact"); r != decider.ReasonAlreadyExists {
		t.Fatalf("want reason %s, got %q", decider.ReasonAlreadyExists, r)
	}
}
