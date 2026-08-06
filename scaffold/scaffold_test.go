package scaffold

import (
	"strings"
	"testing"
)

func sampleDomain() Domain {
	return Domain{
		Aggregate: "ticket",
		Commands: []Command{
			{Name: "OpenTicket", Once: true,
				Fields: []Field{{Name: "subject", Type: "text"}, {Name: "priority", Type: "number"}},
				Events: []Event{{Name: "TicketOpened",
					Fields: []Field{{Name: "subject", Type: "text"}, {Name: "priority", Type: "number"}}}}},
			{Name: "CloseTicket", RequiresExisting: true,
				Fields: []Field{{Name: "resolution", Type: "text"}},
				Events: []Event{{Name: "TicketClosed",
					Fields: []Field{{Name: "resolution", Type: "text"}}}}},
		},
		ReadModels: []ReadModel{{
			Collection: "tickets",
			Key:        "ticketId",
			Fields: []Field{
				{Name: "subject", Type: "text"},
				{Name: "priority", Type: "number"},
				{Name: "resolution", Type: "text"},
			},
		}},
	}
}

// TestGenerateShape: the generated files must declare what the model said,
// in the directives the loader actually reads.
func TestGenerateShape(t *testing.T) {
	files, err := sampleDomain().Generate()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("expected a decider and a projection, got %d files", len(files))
	}

	dec, proj := files[0], files[1]
	if dec.Name != "ticket.js" || dec.Kind != "decider" {
		t.Errorf("decider file: %+v", dec)
	}
	if proj.Name != "tickets.js" || proj.Kind != "projection" {
		t.Errorf("projection file: %+v", proj)
	}

	for _, want := range []string{
		"//@trigger decider ticket",
		"//@handles TicketOpened TicketClosed",
		"//@commands OpenTicket CloseTicket", // the declared surface, from the model
		"case 'OpenTicket':",
		"subject: command.payload.subject",
		"already exists",   // the create is guarded
		"does not exist",   // and the follow-up requires one
		"function evolve(", // the fold
	} {
		if !strings.Contains(dec.Source, want) {
			t.Errorf("decider source missing %q", want)
		}
	}

	for _, want := range []string{
		"//@trigger projection tickets on TicketOpened TicketClosed",
		"//@schema tickets ticketId:text subject:text priority:number resolution:text",
		"//@key ticketId",
		"upsert:", // row ops, not plain rows — the mistake the dry run exists to catch
		"event.aggregateId",
	} {
		if !strings.Contains(proj.Source, want) {
			t.Errorf("projection source missing %q", want)
		}
	}
}

// TestGeneratedProjectionReturnsRowOps guards the specific bug the generator
// must never reproduce: a projection returning plain objects writes nothing,
// silently.
func TestGeneratedProjectionReturnsRowOps(t *testing.T) {
	files, err := sampleDomain().Generate()
	if err != nil {
		t.Fatal(err)
	}
	src := files[1].Source
	if !strings.Contains(src, "return [{ upsert: { key:") {
		t.Fatalf("the generated projection must return row ops:\n%s", src)
	}
}

// TestValidateReportsEveryProblem: a model is described by a human (or by an
// importer) and should come back with all of its problems, not the first.
func TestValidateReportsEveryProblem(t *testing.T) {
	err := Domain{
		Aggregate: "9bad",
		Commands: []Command{
			{Name: "Do It", Fields: []Field{{Name: "x", Type: "guess"}},
				Events: []Event{{Name: "Done", NoFields: true}}},
			{Name: "Other", Once: true, Events: []Event{{Name: "Done", NoFields: true}}},
			{Name: "AlsoCreate", Once: true, Events: []Event{{Name: "Created", NoFields: true}}},
			{Name: "Silent"}, // no events at all
		},
		ReadModels: []ReadModel{{Collection: "ok", Key: "id"}},
	}.Validate()
	if err == nil {
		t.Fatal("expected the model to be refused")
	}
	for _, want := range []string{
		"aggregate name",          // invalid name
		`command 1: name "Do It"`, // space in a command name
		"type \"guess\"",          // unknown field type
		"more than one command",   // two creates
		`command "Silent" records no event`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation did not mention %q: %v", want, err)
		}
	}
}

// TestOneEventFromTwoCommandsIsAllowed: the old model refused this, and the
// refusal would have made a well-formed schema document unimportable for
// what looks like a generator bug — a document may reference one event id
// from several slices.
func TestOneEventFromTwoCommandsIsAllowed(t *testing.T) {
	d := Domain{
		Aggregate: "note",
		Commands: []Command{
			{Name: "Create", Once: true, Events: []Event{{Name: "Touched", NoFields: true}}},
			{Name: "Touch", RequiresExisting: true, Events: []Event{{Name: "Touched", NoFields: true}}},
		},
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("two commands may legitimately produce one event: %v", err)
	}
	// ...and //@handles must not list it twice, or the loader sees a
	// duplicate declaration
	files, err := d.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(files[0].Source, "//@handles Touched\n") {
		t.Errorf("//@handles should de-duplicate:\n%s", files[0].Source)
	}
}

// TestMultiEventCommandIsRunnableAndCounted is the decided behaviour for a
// command with several possible events: the generator does NOT invent the
// branch, emits the first as a runnable placeholder, and the gap is reported
// as a warning rather than buried in a comment nobody reads.
func TestMultiEventCommandIsRunnableAndCounted(t *testing.T) {
	d := Domain{
		Aggregate: "payment",
		Commands: []Command{{
			Name:   "AttemptPayment",
			Fields: []Field{{Name: "amount", Type: "number"}},
			Events: []Event{
				{Name: "PaymentAccepted", Fields: []Field{{Name: "amount", Type: "number"}}},
				{Name: "PaymentRefused", Fields: []Field{{Name: "reason", Type: "text"}}},
			},
		}},
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("several possible events is a legitimate model: %v", err)
	}
	files, err := d.Generate()
	if err != nil {
		t.Fatal(err)
	}
	src := files[0].Source
	if !strings.Contains(src, "//@handles PaymentAccepted PaymentRefused") {
		t.Errorf("both events must be declared:\n%s", src)
	}
	if !strings.Contains(src, "MORE THAN ONE EVENT") || !strings.Contains(src, "PaymentAccepted, PaymentRefused") {
		t.Errorf("the generated code must name the choice it did not make:\n%s", src)
	}
	if !strings.Contains(src, "return [{ type: 'PaymentAccepted'") {
		t.Errorf("the first event should be the runnable placeholder:\n%s", src)
	}
	if strings.Contains(src, "return [{ type: 'PaymentRefused'") {
		t.Errorf("the generator must not invent a branch it cannot know:\n%s", src)
	}

	warnings := d.Warnings()
	var counted bool
	for _, w := range warnings {
		if strings.Contains(w, "AttemptPayment") && strings.Contains(w, "outcome rule") {
			counted = true
		}
	}
	if !counted {
		t.Errorf("a multi-event command must be reported, not just commented: %v", warnings)
	}
}

// TestUnspecifiedPayloadIsWarned: "carries nothing" and "nobody said what it
// carries" must not look alike. An explicit NoFields is what tells them
// apart, and the difference is reported.
func TestUnspecifiedPayloadIsWarned(t *testing.T) {
	unspecified := Domain{
		Aggregate: "thing",
		Commands:  []Command{{Name: "Make", Events: []Event{{Name: "Made"}}}},
	}
	if err := unspecified.Validate(); err != nil {
		t.Fatalf("an unspecified payload must not be a hard error: %v", err)
	}
	warnings := unspecified.Warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "does not say what its payload is") {
		t.Fatalf("expected one payload warning, got %v", warnings)
	}

	declaredEmpty := Domain{
		Aggregate: "thing",
		Commands:  []Command{{Name: "Make", Events: []Event{{Name: "Made", NoFields: true}}}},
	}
	if w := declaredEmpty.Warnings(); len(w) != 0 {
		t.Errorf("an explicitly empty payload is a decision, not a gap: %v", w)
	}
}

// TestEventFieldsAreNotInheritedFromTheCommand: the generator uses what the
// EVENT declares. A front-end that wants the command's fields on the event
// copies them when it builds the model.
func TestEventFieldsAreNotInheritedFromTheCommand(t *testing.T) {
	d := Domain{
		Aggregate: "thing",
		Commands: []Command{{
			Name:   "Make",
			Fields: []Field{{Name: "label", Type: "text"}},
			Events: []Event{{Name: "Made", NoFields: true}},
		}},
	}
	files, err := d.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(files[0].Source, "command.payload.label") {
		t.Errorf("the event declared no payload; the command's fields must not leak onto it:\n%s", files[0].Source)
	}
}

// TestValidateAccepts covers the shapes that must work, including the
// write-only slice (no read model yet).
func TestValidateAccepts(t *testing.T) {
	if err := sampleDomain().Validate(); err != nil {
		t.Errorf("the sample domain should be valid: %v", err)
	}

	writeOnly := Domain{
		Aggregate: "audit",
		Commands:  []Command{{Name: "Record", Events: []Event{{Name: "Recorded", NoFields: true}}}},
	}
	if err := writeOnly.Validate(); err != nil {
		t.Errorf("a slice with no read model is legitimate: %v", err)
	}
	files, err := writeOnly.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("a write-only slice generates only a decider, got %d files", len(files))
	}
}

// TestSeveralReadModels: a schema document routinely declares more than one
// read model over one aggregate's events, and each becomes its own
// projection file.
func TestSeveralReadModels(t *testing.T) {
	d := sampleDomain()
	d.ReadModels = append(d.ReadModels, ReadModel{
		Collection: "openTickets", Key: "ticketId",
		Fields: []Field{{Name: "subject", Type: "text"}},
		On:     []string{"TicketOpened"},
	})
	files, err := d.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("expected a decider and two projections, got %d", len(files))
	}
	if !strings.Contains(files[2].Source, "//@trigger projection openTickets on TicketOpened\n") {
		t.Errorf("the second read model kept its own trigger list:\n%s", files[2].Source)
	}

	// two read models writing one collection is a real conflict
	d.ReadModels = append(d.ReadModels, ReadModel{Collection: "tickets", Key: "ticketId"})
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Errorf("two read models on one collection must be refused, got %v", err)
	}
}

// TestReactorGeneration: the automation shape, and the idempotency pattern
// baked into what is generated.
func TestReactorGeneration(t *testing.T) {
	d := sampleDomain()
	d.Reactors = []Reactor{{
		Name: "autoClose", On: []string{"TicketOpened"},
		Aggregate: "audit", Command: "Record", IDPrefix: "audit-",
	}}
	files, err := d.Generate()
	if err != nil {
		t.Fatal(err)
	}
	last := files[len(files)-1]
	if last.Name != "autoClose.js" || last.Kind != "reactor" {
		t.Fatalf("unexpected reactor file: %+v", last)
	}
	for _, want := range []string{
		"//@trigger reactor TicketOpened",
		"//@dispatches audit/Record",
		"function reactTo(event)",
		"aggregate: 'audit'",
		"id: 'audit-' + event.aggregateId", // deterministic: replay-safe
		"command: 'Record'",
	} {
		if !strings.Contains(last.Source, want) {
			t.Errorf("reactor source missing %q:\n%s", want, last.Source)
		}
	}

	// a reactor with no triggers can never fire, which is worth refusing
	d.Reactors[0].On = nil
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "never fire") {
		t.Errorf("a trigger-less reactor must be refused, got %v", err)
	}
}

// TestReadModelKeyIsAlwaysAColumn: the key must exist in the schema whether
// or not whoever described the model remembered to list it.
func TestReadModelKeyIsAlwaysAColumn(t *testing.T) {
	d := Domain{
		Aggregate: "thing",
		Commands: []Command{{Name: "Make",
			Events: []Event{{Name: "Made", Fields: []Field{{Name: "label", Type: "text"}}}}}},
		ReadModels: []ReadModel{{Collection: "things", Key: "thingId",
			Fields: []Field{{Name: "label", Type: "text"}}}},
	}
	files, err := d.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(files[1].Source, "//@schema things thingId:text label:text") {
		t.Errorf("the key column was not added:\n%s", files[1].Source)
	}
	// and it is not duplicated when it WAS listed
	d.ReadModels[0].Fields = append([]Field{{Name: "thingId", Type: "text"}}, d.ReadModels[0].Fields...)
	files, err = d.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(files[1].Source, "thingId:text") != 1 {
		t.Errorf("the key column was duplicated:\n%s", files[1].Source)
	}
}
