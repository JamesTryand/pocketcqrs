package emschema

import (
	"strings"
	"testing"
)

var consultingDoc = strings.NewReplacer(
	`"automationId":"on-sale",`, `"automationId":"on-sale","readModelId":"pending-shipments",`,
	`"automations":`, `"readModels":{"pending-shipments":{"name":"Pending Shipments","builtFromEventIds":["sale-logged"],
		"fields":[{"name":"saleId","type":"string","idAttribute":true}]}},
	"automations":`).Replace(automationUpsertDoc)

// An automation that consults a read model is told where the facts come
// from instead, not invited to read the read side from reactTo(), which no
// longer has `pb`.
func TestAnAutomationConsultingAReadModelIsNotToldToReadIt(t *testing.T) {
	doc, err := Load(writeDoc(t, consultingDoc))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var said string
	for _, w := range mapped.Report.Warnings {
		if strings.Contains(w, "pending-shipments") {
			said = w
		}
	}
	if said == "" {
		t.Fatalf("no warning names the consulted read model: %v", mapped.Report.Warnings)
	}
	if strings.Contains(said, "pb.") || !strings.Contains(said, "event") || !strings.Contains(said, "decider") {
		t.Fatalf("the warning must point at the event and the target decider, not pb: %s", said)
	}
}
