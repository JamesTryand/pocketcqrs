package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// refusalDomain: an entry whose updates are refused while it is on an
// invoice, for non-positive or over-a-day hours, for a frozen invoice, for a
// tag it already has; and a claim refused while another invoice holds it.
func refusalDomain() Domain {
	return Domain{
		Aggregate: "entry",
		Commands: []Command{
			{Name: "LogEntry", Once: true,
				Fields: []Field{{Name: "hours", Type: "number"}, {Name: "tags", Type: "json"}},
				Events: []Event{{Name: "EntryLogged", Fields: []Field{{Name: "hours", Type: "number"}, {Name: "tags", Type: "json"}}}}},
			{Name: "ClaimEntry", RequiresExisting: true,
				Fields: []Field{{Name: "invoiceId", Type: "text"}},
				Events: []Event{{Name: "EntryClaimed", Fields: []Field{{Name: "claimedFor", Type: "text"}}}},
				Refusals: []Refusal{{Message: "entry is on another invoice", All: []RefusalCondition{
					{Side: "state", Field: "claimedFor", Op: "set"},
					{Side: "state", Field: "claimedFor", Op: "notEquals", Operand: &RefusalOperand{Side: "command", Field: "invoiceId"}},
				}}}},
			{Name: "UpdateEntry", RequiresExisting: true,
				Fields: []Field{{Name: "hours", Type: "number"}, {Name: "tag", Type: "text"}, {Name: "invoiceId", Type: "text"}},
				Events: []Event{{Name: "EntryUpdated", Fields: []Field{{Name: "hours", Type: "number"}}}},
				Refusals: []Refusal{
					{Message: "entry is on an invoice", All: []RefusalCondition{{Side: "state", Field: "claimedFor", Op: "set"}}},
					{Message: "hours must be positive", All: []RefusalCondition{{Side: "command", Field: "hours", Op: "atMost", Operand: &RefusalOperand{Literal: 0.0}}}},
					{Message: "a day has at most 24 hours", All: []RefusalCondition{{Side: "command", Field: "hours", Op: "greaterThan", Operand: &RefusalOperand{Literal: 24.0}}}},
					{Message: "not more than logged", All: []RefusalCondition{{Side: "command", Field: "hours", Op: "greaterThan", Operand: &RefusalOperand{Side: "state", Field: "hours"}}}},
					{Message: "invoice is frozen", All: []RefusalCondition{{Side: "command", Field: "invoiceId", Op: "equals", Operand: &RefusalOperand{Literal: "frozen"}}}},
					{Message: "already tagged", All: []RefusalCondition{{Side: "state", Field: "tags", Op: "contains", Operand: &RefusalOperand{Side: "command", Field: "tag"}}}},
				}},
		},
	}
}

const refusalBehaviour = `package entry

import (
	"encoding/json"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

func fold(t *testing.T, evs ...events.Event) State {
	d := Entry()
	s := d.InitialState()
	for _, ev := range evs {
		var err error
		if s, err = d.Evolve(s, ev); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func refusal(t *testing.T, s State, name, payload string) string {
	_, err := Entry().Decide(decider.Command{Name: name, Payload: json.RawMessage(payload)}, s)
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestRefusals(t *testing.T) {
	logged := events.Event{Type: EntryLogged, Data: json.RawMessage(` + "`" + `{"hours":8,"tags":["night"]}` + "`" + `)}
	claimed := events.Event{Type: EntryClaimed, Data: json.RawMessage(` + "`" + `{"claimedFor":"inv-1"}` + "`" + `)}
	open := fold(t, logged)
	onInvoice := fold(t, logged, claimed)
	for _, c := range []struct{ s State; name, payload, want string }{
		{open, "UpdateEntry", ` + "`" + `{"hours":7}` + "`" + `, ""},
		{onInvoice, "UpdateEntry", ` + "`" + `{"hours":7}` + "`" + `, "entry is on an invoice"},
		{open, "UpdateEntry", ` + "`" + `{"hours":0}` + "`" + `, "hours must be positive"},
		{open, "UpdateEntry", ` + "`" + `{"hours":-2}` + "`" + `, "hours must be positive"},
		{open, "UpdateEntry", ` + "`" + `{"hours":25}` + "`" + `, "a day has at most 24 hours"},
		{open, "UpdateEntry", ` + "`" + `{"hours":9}` + "`" + `, "not more than logged"},
		{open, "UpdateEntry", ` + "`" + `{}` + "`" + `, ""},
		{open, "UpdateEntry", ` + "`" + `{"hours":1,"invoiceId":"frozen"}` + "`" + `, "invoice is frozen"},
		{open, "UpdateEntry", ` + "`" + `{"hours":1,"tag":"night"}` + "`" + `, "already tagged"},
		{open, "UpdateEntry", ` + "`" + `{"hours":1,"tag":"day"}` + "`" + `, ""},
		{open, "ClaimEntry", ` + "`" + `{"invoiceId":"inv-2"}` + "`" + `, ""},
		{onInvoice, "ClaimEntry", ` + "`" + `{"invoiceId":"inv-1"}` + "`" + `, ""},
		{onInvoice, "ClaimEntry", ` + "`" + `{"invoiceId":"inv-2"}` + "`" + `, "entry is on another invoice"},
	} {
		if got := refusal(t, c.s, c.name, c.payload); got != c.want {
			t.Errorf("%s %s: got %q, want %q", c.name, c.payload, got, c.want)
		}
	}
}
`

// The Go target's refusals, run: the generated decider is built in its own
// module with a test beside it that folds events and checks every rule's
// message, and that an unset value (no hours sent) never trips a numeric
// rule.
func TestGoRefusalsRun(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	files, err := refusalDomain().GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	goMod := "module refusal-domain\n\ngo 1.25.0\n\nrequire github.com/jamestryand/pocketcqrs v0.0.0\n\n" +
		"replace github.com/jamestryand/pocketcqrs => " + repoRoot + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.Name), []byte(f.Source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "refusal_behaviour_test.go"), []byte(refusalBehaviour), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir, tidy.Env = dir, env
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	run := exec.Command("go", "test", "./...")
	run.Dir, run.Env = dir, env
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("the generated refusals misbehave:\n%s", out)
	}
}

// A rule naming a field that isn't there, or comparing one the wrong way,
// is refused by Validate rather than generated as a rule that never fires.
func TestRefusalFieldsAreChecked(t *testing.T) {
	for name, cond := range map[string]RefusalCondition{
		"unknown state field":   {Side: "state", Field: "ghost", Op: "set"},
		"unknown command field": {Side: "command", Field: "ghost", Op: "set"},
		"numeric on text":       {Side: "command", Field: "invoiceId", Op: "atLeast", Operand: &RefusalOperand{Literal: 1.0}},
		"contains on a number":  {Side: "state", Field: "hours", Op: "contains", Operand: &RefusalOperand{Literal: 1.0}},
	} {
		d := refusalDomain()
		d.Commands[2].Refusals = []Refusal{{Message: "x", All: []RefusalCondition{cond}}}
		if err := d.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
