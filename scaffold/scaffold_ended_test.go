package scaffold

import (
	"strings"
	"testing"
)

// A create that may begin its stream again after an end (CanBeginAgain, an
// assign after an unassign) leaves the end not final, so neither generator
// adds the Ended flag: the aggregate generates exactly as before D28.
func TestCanBeginAgainGeneratesNoEndedFlag(t *testing.T) {
	d := finding3Domain()
	d.Commands[0].CanBeginAgain = true // CreateStaff
	js, err := d.Generate()
	if err != nil {
		t.Fatal(err)
	}
	goFiles, err := d.GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(js[0].Source, "ended") {
		t.Errorf("JS decider has an ended flag:\n%s", js[0].Source)
	}
	if strings.Contains(goFiles[0].Source, "Ended") {
		t.Errorf("Go decider has an Ended flag:\n%s", goFiles[0].Source)
	}
	if !strings.Contains(js[0].Source, "case 'StaffRemoved':\n      return Object.assign({}, state, event.data, { exists: false });") {
		t.Errorf("StaffRemoved should still reset exists:\n%s", js[0].Source)
	}
}

// An aggregate with a create but nothing that ends its stream has no end to
// be final, so it gets no Ended flag either.
func TestNoEndsStreamGeneratesNoEndedFlag(t *testing.T) {
	d := Domain{
		Aggregate: "note",
		Commands: []Command{
			{Name: "CreateNote", Once: true, Events: []Event{{Name: "NoteCreated", NoFields: true}}},
			{Name: "EditNote", RequiresExisting: true, Events: []Event{{Name: "NoteEdited", NoFields: true}}},
		},
	}
	js, err := d.Generate()
	if err != nil {
		t.Fatal(err)
	}
	goFiles, err := d.GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(js[0].Source, "ended") || strings.Contains(goFiles[0].Source, "Ended") {
		t.Errorf("no stream end, so no ended flag:\n%s\n%s", js[0].Source, goFiles[0].Source)
	}
}
