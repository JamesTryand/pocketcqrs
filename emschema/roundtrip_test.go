package emschema

import (
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/catalog"
	"github.com/jamestryand/pocketcqrs/scaffold"
)

// TestImportWorkedExample maps the real worked example and asserts what the
// mapping is supposed to produce — including the two things it must refuse
// to guess at.
func TestImportWorkedExample(t *testing.T) {
	doc, err := Load(fixture(t, "examples/order-fulfillment.json"))
	if err != nil {
		t.Fatal(err)
	}

	// The worked example deliberately leaves `notify-shipping-partner`
	// untagged, so a bare import MUST refuse rather than invent an
	// aggregate. This is the decided behaviour, and the message has to say
	// how to proceed.
	_, err = Map(doc, Options{})
	if err == nil {
		t.Fatal("an untagged command must be refused, not guessed")
	}
	if !strings.Contains(err.Error(), "notify-shipping-partner") || !strings.Contains(err.Error(), "--aggregate") {
		t.Fatalf("the refusal must name the element and how to resolve it: %v", err)
	}

	// with the operator's decision supplied, it maps
	mapped, err := Map(doc, Options{AggregateOverrides: map[string]string{
		"notify-shipping-partner": "shipmentNotice",
	}})
	if err != nil {
		t.Fatalf("with an override supplied the document must import: %v", err)
	}

	byName := map[string]int{}
	for i, d := range mapped.Domains {
		byName[d.Aggregate] = i
	}
	order, ok := byName["order"]
	if !ok {
		t.Fatalf("expected an order aggregate, got %v", byName)
	}
	orderDomain := mapped.Domains[order]

	// three commands touch Order: place, ship (from the automation's target)
	// — the automation contributes its dispatched command to the target
	var names []string
	for _, c := range orderDomain.Commands {
		names = append(names, c.Name)
	}
	if len(names) != 2 {
		t.Fatalf("expected PlaceOrder and ShipOrder on Order, got %v", names)
	}

	// the id/name pair folded into one identifier
	var placed bool
	for _, c := range orderDomain.Commands {
		if c.Name == "PlaceOrder" {
			placed = true
			if len(c.Events) != 1 || c.Events[0].Name != "OrderPlaced" {
				t.Errorf("PlaceOrder should record OrderPlaced, got %+v", c.Events)
			}
			// order-placed's `items` field is a LIST with subfields, so it
			// must fold to json rather than being dropped or mangled
			var itemsType string
			for _, f := range c.Events[0].Fields {
				if f.Name == "items" {
					itemsType = f.Type
				}
			}
			if itemsType != "json" {
				t.Errorf("a list-with-subfields field must fold to json, got %q", itemsType)
			}
		}
	}
	if !placed {
		t.Errorf("PlaceOrder not mapped: %v", names)
	}

	// the read models land as projections, keyed on the idAttribute field
	var summary bool
	for _, rm := range orderDomain.ReadModels {
		if rm.Collection == "orderSummary" {
			summary = true
			if rm.Key != "orderId" {
				t.Errorf("idAttribute should have supplied the key, got %q", rm.Key)
			}
			if len(rm.On) != 2 {
				t.Errorf("orderSummary folds two events, got %v", rm.On)
			}
		}
	}
	if !summary {
		t.Errorf("orderSummary was not mapped: %+v", orderDomain.ReadModels)
	}

	// the automations became reactors
	var reactors int
	for _, d := range mapped.Domains {
		reactors += len(d.Reactors)
	}
	if reactors != 2 {
		t.Errorf("both automation slices should become reactors, got %d", reactors)
	}

	// every decision taken on the document's behalf is named
	if len(mapped.Report.Warnings) == 0 {
		t.Error("a mapping that folded types and supplied an override must report it")
	}
	joined := strings.Join(mapped.Report.Warnings, "\n")
	if !strings.Contains(joined, "override") {
		t.Errorf("the supplied override must be reported: %v", mapped.Report.Warnings)
	}

	// and the domain doc carries the prose that code cannot
	orderDoc := mapped.DomainDocs["order"]
	if !strings.Contains(orderDoc, "A customer wants to buy items.") {
		t.Errorf("Command.reason must reach the domain doc:\n%s", orderDoc)
	}
	if !strings.Contains(orderDoc, "What's the current status of my order?") {
		t.Errorf("ReadModel.question must reach the domain doc:\n%s", orderDoc)
	}
	if !strings.Contains(orderDoc, "unclear-status-timing") {
		t.Errorf("hotspots must reach the domain doc:\n%s", orderDoc)
	}
}

// TestGeneratedSliceLoads: every file the importer produces must actually
// load, or the import has only pretended to work.
func TestGeneratedSliceLoads(t *testing.T) {
	doc, err := Load(fixture(t, "examples/order-fulfillment.json"))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{AggregateOverrides: map[string]string{
		"notify-shipping-partner": "shipmentNotice",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range mapped.Domains {
		files, err := d.Generate()
		if err != nil {
			t.Fatalf("%s did not generate: %v", d.Aggregate, err)
		}
		for _, f := range files {
			if !strings.HasPrefix(f.Source, "//@trigger ") {
				t.Errorf("%s does not start with a trigger directive:\n%s", f.Name, f.Source)
			}
		}
	}
}

// TestRoundTripLoss is the loss MEASUREMENT: import a document, export what
// a platform running it would report, and state exactly what survived.
//
// It is not a fidelity promise, and the assertions below are deliberately
// about the recoverable core rather than equality — the interesting output
// of this test is the documented list of what does NOT come back.
func TestRoundTripLoss(t *testing.T) {
	doc, err := Load(fixture(t, "examples/order-fulfillment.json"))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{AggregateOverrides: map[string]string{
		"notify-shipping-partner": "shipmentNotice",
	}})
	if err != nil {
		t.Fatal(err)
	}

	// stand in for a running platform that has loaded the imported slice:
	// declared commands and handled events are what the catalog reports
	cat := &catalog.Catalog{}
	for _, d := range mapped.Domains {
		agg := catalog.Aggregate{Name: d.Aggregate, Origin: "js", Produces: map[string][]string{}}
		for _, c := range d.Commands {
			agg.Commands = append(agg.Commands, c.Name)
			for _, e := range c.Events {
				agg.Produces[c.Name] = append(agg.Produces[c.Name], e.Name)
			}
		}
		agg.Handles = d.Events()
		cat.Aggregates = append(cat.Aggregates, agg)
		for _, rm := range d.ReadModels {
			cat.Consumers = append(cat.Consumers, catalog.Consumer{
				Name: rm.Collection, Kind: "js-projection",
				Collections: []string{rm.Collection}, EventTypes: rm.On,
			})
		}
		for _, r := range d.Reactors {
			cat.Consumers = append(cat.Consumers, catalog.Consumer{
				Name: "fn-react:" + r.Name, Kind: "js-reactor",
				EventTypes: r.On, Dispatches: []string{r.Aggregate + "/" + r.Command},
			})
		}
	}

	out, rep := FromCatalog(cat)

	// ---- the recoverable core ----

	// every event name comes back, because it is declared AND in the log
	original := map[string]bool{}
	for _, d := range mapped.Domains {
		for _, e := range d.Events() {
			original[DeriveID(e)] = true
		}
	}
	for id := range original {
		if _, ok := out.Events[id]; !ok {
			t.Errorf("event %q did not survive the round trip", id)
		}
	}

	// every command name comes back, because DASH.6's declared surface
	// records what the log cannot
	for _, d := range mapped.Domains {
		for _, c := range d.Commands {
			if _, ok := out.Commands[DeriveID(c.Name)]; !ok {
				t.Errorf("command %q did not survive the round trip", c.Name)
			}
		}
	}

	// read models and their source events come back
	for _, d := range mapped.Domains {
		for _, rm := range d.ReadModels {
			id := DeriveID(TypeName(rm.Collection, rm.Collection))
			exported, ok := out.ReadModels[id]
			if !ok {
				t.Errorf("read model %q did not survive", rm.Collection)
				continue
			}
			if len(exported.BuiltFromEventIDs) != len(rm.On) {
				t.Errorf("read model %q lost source events: %v vs %v",
					rm.Collection, exported.BuiltFromEventIDs, rm.On)
			}
		}
	}

	// the exported document must be structurally valid on its own terms —
	// swimlanes and screens were SYNTHESIZED, not omitted, because omitting
	// a required property makes the document invalid rather than lossy
	if lerr := Lint(out).Err(); lerr != nil {
		t.Fatalf("the exported document does not satisfy its own schema: %v", lerr)
	}
	if len(out.Swimlanes) != 1 {
		t.Errorf("export must synthesize exactly one swimlane, got %d", len(out.Swimlanes))
	}
	for _, s := range out.Slices {
		if s.Pattern == PatternAutomation && s.ScreenID != "" {
			t.Errorf("slice %q is an automation and must NOT carry a screenId: "+
				"unevaluatedProperties would reject the document", s.ID)
		}
		if s.Pattern != PatternAutomation && s.ScreenID == "" {
			t.Errorf("slice %q requires a synthesized screen", s.ID)
		}
	}

	// ---- the documented loss ----

	lossy := strings.Join(append(append([]string{}, rep.Warnings...), rep.Lossy...), "\n")
	for _, mustMention := range []string{
		"swimlane", // synthesized
		"reason",   // methodology prose
		"chapters", // board state
	} {
		if !strings.Contains(lossy, mustMention) {
			t.Errorf("the report must name %q as lost or synthesized:\n%s", mustMention, lossy)
		}
	}

	// The gap //@produces closed, now asserted from the other side: an
	// exported slice must list EXACTLY the events its command produces, not
	// the aggregate's whole set. Before the directive existed this test
	// asserted the widening instead.
	for _, s := range out.Slices {
		if s.Pattern != PatternStateChange {
			continue
		}
		var originalWidth int
		for _, orig := range doc.Slices {
			if orig.Pattern == PatternStateChange && DeriveID(TypeName(
				doc.Commands[orig.CommandID].Name, orig.CommandID)) == s.CommandID {
				originalWidth = len(orig.EventIDs)
			}
		}
		if originalWidth > 0 && len(s.EventIDs) != originalWidth {
			t.Errorf("slice %q should round-trip %d event(s), got %d (%v): //@produces exists "+
				"precisely so this no longer widens", s.ID, originalWidth, len(s.EventIDs), s.EventIDs)
		}
	}
}

// TestVerifyRunsScenarios turns the worked example's scenarios into real
// checks against the code the import generated — the step that makes a
// scenario more than documentation.
func TestVerifyRunsScenarios(t *testing.T) {
	doc, err := Load(fixture(t, "examples/order-fulfillment.json"))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{AggregateOverrides: map[string]string{
		"notify-shipping-partner": "shipmentNotice",
	}})
	if err != nil {
		t.Fatal(err)
	}
	results, err := Verify(doc, mapped, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 4 {
		t.Fatalf("the worked example has four scenarios, got %d", len(results))
	}

	byID := map[string]ScenarioResult{}
	for _, r := range results {
		byID[r.ScenarioID] = r
	}

	// The happy path must actually pass: given nothing, PlaceOrder appends
	// OrderPlaced. If this fails the generated decider does not do what the
	// document says, which is the whole point of running these.
	happy := byID["place-order-happy-path"]
	if !happy.Passed {
		t.Errorf("the happy-path scenario should pass against the generated code: %s", happy.Detail)
	}
	// ...and it reports the data divergence without failing on it. The
	// document's example says OrderPlaced carries orderId and customerId;
	// the event DECLARES orderId, customerEmail and items, and the generated
	// decider copies only declared fields from the command payload. The
	// source schema says outright that scenario data is not cross-checked
	// against declared fields, so this must be a note, not a failure.
	if !strings.Contains(happy.Detail, "example data differs") {
		t.Errorf("the data divergence should be reported: %s", happy.Detail)
	}

	// ...and one that exercises a rejection is meaningful only because a
	// refusal is a verdict rather than an error. A create applied twice is
	// the generated decider's own rule.
	twice := Scenario{
		ID: "create-twice", Name: "placing an order twice is refused", Kind: KindError,
		Given: []EventRef{{EventID: "order-placed", Data: []byte(`{"orderId":"o1"}`)}},
		When:  []byte(`{"commandId":"place-order"}`),
		Then:  []byte(`{"error":{"message":"already exists"}}`),
	}
	v := &verifier{doc: doc, dir: t.TempDir(), sources: map[string]scaffold.File{}}
	if err := v.indexSources(mapped); err != nil {
		t.Fatal(err)
	}
	got := v.run(Slice{ID: "synthetic"}, twice)
	if !got.Passed {
		t.Errorf("a repeated create should be refused by the generated decider: %s", got.Detail)
	}

	// The stateView scenario must FAIL, and its failure is the useful kind:
	// order-summary declares a `status` field, no event carries one, so the
	// generated projection cannot produce it. That is a rule the document
	// describes and nobody has written — exactly what running scenarios is
	// for. If this ever passes, the generator started inventing values.
	view := byID["view-order-summary-after-placement"]
	if view.Passed {
		t.Error("the stateView scenario expects a derived `status` no event carries; it must not pass")
	}
	if !strings.Contains(view.Detail, "status") {
		t.Errorf("the failure should name the missing field: %s", view.Detail)
	}

	// a scenario that contradicts the generated code must FAIL, or these
	// checks would pass vacuously for any code at all
	wrong := Scenario{
		ID: "wrong-event", Name: "expects an event the command does not append", Kind: KindStateChange,
		Given: []EventRef{},
		When:  []byte(`{"commandId":"place-order"}`),
		Then:  []byte(`{"events":[{"eventId":"order-shipped"}]}`),
	}
	if bad := v.run(Slice{ID: "synthetic"}, wrong); bad.Passed {
		t.Error("a scenario expecting the wrong event must fail")
	}
}
