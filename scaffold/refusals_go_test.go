package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// refusalDomain: a booking whose changes are refused while an event plan
// holds it, for a non-positive or over-a-day duration, for a frozen event
// plan, for a tag it already has; and a hold refused while another event
// plan holds it.
func refusalDomain() Domain {
	return Domain{
		Aggregate: "booking",
		Commands: []Command{
			{Name: "MakeBooking", Once: true,
				Fields: []Field{{Name: "duration", Type: "number"}, {Name: "tags", Type: "json"}},
				Events: []Event{{Name: "BookingMade", Fields: []Field{{Name: "duration", Type: "number"}, {Name: "tags", Type: "json"}}}}},
			{Name: "HoldBooking", RequiresExisting: true,
				Fields: []Field{{Name: "planId", Type: "text"}},
				Events: []Event{{Name: "BookingHeld", Fields: []Field{{Name: "heldFor", Type: "text"}}}},
				Refusals: []Refusal{{Message: "booking is held by another event plan", All: []RefusalCondition{
					{Side: "state", Field: "heldFor", Op: "set"},
					{Side: "state", Field: "heldFor", Op: "notEquals", Operand: &RefusalOperand{Side: "command", Field: "planId"}},
				}}}},
			{Name: "ChangeBooking", RequiresExisting: true,
				Fields: []Field{{Name: "duration", Type: "number"}, {Name: "tag", Type: "text"}, {Name: "planId", Type: "text"}},
				Events: []Event{{Name: "BookingChanged", Fields: []Field{{Name: "duration", Type: "number"}}}},
				Refusals: []Refusal{
					{Message: "booking is held by an event plan", All: []RefusalCondition{{Side: "state", Field: "heldFor", Op: "set"}}},
					{Message: "duration must be positive", All: []RefusalCondition{{Side: "command", Field: "duration", Op: "atMost", Operand: &RefusalOperand{Literal: 0.0}}}},
					{Message: "a booking lasts at most one day", All: []RefusalCondition{{Side: "command", Field: "duration", Op: "greaterThan", Operand: &RefusalOperand{Literal: 24.0}}}},
					{Message: "not more than booked", All: []RefusalCondition{{Side: "command", Field: "duration", Op: "greaterThan", Operand: &RefusalOperand{Side: "state", Field: "duration"}}}},
					{Message: "event plan is frozen", All: []RefusalCondition{{Side: "command", Field: "planId", Op: "equals", Operand: &RefusalOperand{Literal: "frozen"}}}},
					{Message: "already tagged", All: []RefusalCondition{{Side: "state", Field: "tags", Op: "contains", Operand: &RefusalOperand{Side: "command", Field: "tag"}}}},
				}},
		},
	}
}

const refusalBehaviour = `package booking

import (
	"encoding/json"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

func fold(t *testing.T, evs ...events.Event) State {
	d := Booking()
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
	_, err := Booking().Decide(decider.Command{Name: name, Payload: json.RawMessage(payload)}, s)
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestRefusals(t *testing.T) {
	logged := events.Event{Type: BookingMade, Data: json.RawMessage(` + "`" + `{"duration":8,"tags":["night"]}` + "`" + `)}
	claimed := events.Event{Type: BookingHeld, Data: json.RawMessage(` + "`" + `{"heldFor":"plan-1"}` + "`" + `)}
	open := fold(t, logged)
	held := fold(t, logged, claimed)
	for _, c := range []struct{ s State; name, payload, want string }{
		{open, "ChangeBooking", ` + "`" + `{"duration":7}` + "`" + `, ""},
		{held, "ChangeBooking", ` + "`" + `{"duration":7}` + "`" + `, "booking is held by an event plan"},
		{open, "ChangeBooking", ` + "`" + `{"duration":0}` + "`" + `, "duration must be positive"},
		{open, "ChangeBooking", ` + "`" + `{"duration":-2}` + "`" + `, "duration must be positive"},
		{open, "ChangeBooking", ` + "`" + `{"duration":25}` + "`" + `, "a booking lasts at most one day"},
		{open, "ChangeBooking", ` + "`" + `{"duration":9}` + "`" + `, "not more than booked"},
		{open, "ChangeBooking", ` + "`" + `{}` + "`" + `, ""},
		{open, "ChangeBooking", ` + "`" + `{"duration":1,"planId":"frozen"}` + "`" + `, "event plan is frozen"},
		{open, "ChangeBooking", ` + "`" + `{"duration":1,"tag":"night"}` + "`" + `, "already tagged"},
		{open, "ChangeBooking", ` + "`" + `{"duration":1,"tag":"day"}` + "`" + `, ""},
		{open, "HoldBooking", ` + "`" + `{"planId":"plan-2"}` + "`" + `, ""},
		{held, "HoldBooking", ` + "`" + `{"planId":"plan-1"}` + "`" + `, ""},
		{held, "HoldBooking", ` + "`" + `{"planId":"plan-2"}` + "`" + `, "booking is held by another event plan"},
	} {
		if got := refusal(t, c.s, c.name, c.payload); got != c.want {
			t.Errorf("%s %s: got %q, want %q", c.name, c.payload, got, c.want)
		}
	}
}
`

// The Go target's refusals, run: the generated decider is built in its own
// module with a test beside it that folds events and checks every rule's
// message, and that an unset value (no duration sent) never trips a numeric
// rule.
func TestGoRefusalsRun(t *testing.T) {
	runGeneratedGo(t, refusalDomain(), refusalBehaviour)
}

// runGeneratedGo generates d for the Go target into its own module, with
// behaviour beside it as a test file, and runs `go test` there: the
// generated code compiled and exercised, not just read.
func runGeneratedGo(t *testing.T, d Domain, behaviour string) {
	t.Helper()
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	files, err := d.GenerateGo()
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
	if err := os.WriteFile(filepath.Join(dir, "behaviour_test.go"), []byte(behaviour), 0o644); err != nil {
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
		t.Fatalf("the generated code misbehaves:\n%s", out)
	}
}

// A rule naming a field that isn't there, or comparing one the wrong way,
// is refused by Validate rather than generated as a rule that never fires.
func TestRefusalFieldsAreChecked(t *testing.T) {
	for name, cond := range map[string]RefusalCondition{
		"unknown state field":   {Side: "state", Field: "ghost", Op: "set"},
		"unknown command field": {Side: "command", Field: "ghost", Op: "set"},
		"numeric on text":       {Side: "command", Field: "planId", Op: "atLeast", Operand: &RefusalOperand{Literal: 1.0}},
		"contains on a number":  {Side: "state", Field: "duration", Op: "contains", Operand: &RefusalOperand{Literal: 1.0}},
	} {
		d := refusalDomain()
		d.Commands[2].Refusals = []Refusal{{Message: "x", All: []RefusalCondition{cond}}}
		if err := d.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
