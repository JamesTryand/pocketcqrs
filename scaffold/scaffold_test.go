package scaffold

import (
	"strings"
	"testing"
)

func sampleDomain() Domain {
	return Domain{
		Aggregate: "ticket",
		Commands: []Command{
			{Name: "OpenTicket", Event: "TicketOpened", Once: true,
				Fields: []Field{{Name: "subject", Type: "text"}, {Name: "priority", Type: "number"}}},
			{Name: "CloseTicket", Event: "TicketClosed", RequiresExisting: true,
				Fields: []Field{{Name: "resolution", Type: "text"}}},
		},
		ReadModel: &ReadModel{
			Collection: "tickets",
			Key:        "ticketId",
			Fields: []Field{
				{Name: "subject", Type: "text"},
				{Name: "priority", Type: "number"},
				{Name: "resolution", Type: "text"},
			},
		},
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
			{Name: "Do It", Event: "Done", Fields: []Field{{Name: "x", Type: "guess"}}},
			{Name: "Other", Event: "Done"},
		},
		ReadModel: &ReadModel{Collection: "ok", Key: "id", On: []string{"NeverProduced"}},
	}.Validate()
	if err == nil {
		t.Fatal("expected the model to be refused")
	}
	for _, want := range []string{
		"aggregate name",          // invalid name
		`command 1: name "Do It"`, // space in a command name
		"type \"guess\"",          // unknown field type
		"more than one command",   // two commands, one event
		"NeverProduced",           // a read model listening for nothing
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation did not mention %q: %v", want, err)
		}
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
		Commands:  []Command{{Name: "Record", Event: "Recorded"}},
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

// TestReadModelKeyIsAlwaysAColumn: the key must exist in the schema whether
// or not whoever described the model remembered to list it.
func TestReadModelKeyIsAlwaysAColumn(t *testing.T) {
	d := Domain{
		Aggregate: "thing",
		Commands:  []Command{{Name: "Make", Event: "Made", Fields: []Field{{Name: "label", Type: "text"}}}},
		ReadModel: &ReadModel{Collection: "things", Key: "thingId",
			Fields: []Field{{Name: "label", Type: "text"}}},
	}
	files, err := d.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(files[1].Source, "//@schema things thingId:text label:text") {
		t.Errorf("the key column was not added:\n%s", files[1].Source)
	}
	// and it is not duplicated when it WAS listed
	d.ReadModel.Fields = append([]Field{{Name: "thingId", Type: "text"}}, d.ReadModel.Fields...)
	files, err = d.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(files[1].Source, "thingId:text") != 1 {
		t.Errorf("the key column was duplicated:\n%s", files[1].Source)
	}
}
