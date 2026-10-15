// Package wire is what schema verify's Go harness reads and writes: the
// decides and projection runs a document's scenarios need, and the Go
// code's answers. emschema writes the input and compares the output; the
// harness (package verifyharness, compiled with the code under test) only
// runs. It has no PocketBase dependency, so the CLI doesn't carry the
// harness's test app.
package wire

import (
	"encoding/json"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

// EnvIn and EnvOut name the files the harness reads and writes.
const (
	EnvIn  = "PCQ_HARNESS_IN"
	EnvOut = "PCQ_HARNESS_OUT"
)

// Input is everything one harness run answers.
type Input struct {
	Commands    []Command    `json:"commands"`
	Views       []View       `json:"views"`
	Collections []Collection `json:"collections"`
}

// Command is one command scenario's decide: its stream's events folded,
// then the command decided, nothing appended.
type Command struct {
	ID        string          `json:"id"`
	Aggregate string          `json:"aggregate"`
	Stream    string          `json:"stream"`
	Given     []events.Event  `json:"given"`
	Command   decider.Command `json:"command"`
	Meta      map[string]any  `json:"meta"`
}

// View is one view scenario's projection runs, each over its own fixture
// (a read model's rows are keyed by its own id field), in one app.
type View struct {
	ID   string `json:"id"`
	Runs []Run  `json:"runs"`
}

// Run is one projection over its fixture.
type Run struct {
	Collection string         `json:"collection"`
	Events     []events.Event `json:"events"`
}

// Collection is one read model's collection, created in every view's app.
type Collection struct {
	Name   string  `json:"name"`
	Key    string  `json:"key"`
	Fields []Field `json:"fields"`
}

// Field is a column: Type is a //@schema type; PII columns hold the
// envelope's JSON text and are revealed before the rows are returned.
type Field struct {
	Name string `json:"name"`
	Type string `json:"type"`
	PII  bool   `json:"pii,omitempty"`
}

// Output is the harness's answers, by scenario id.
type Output struct {
	Commands []CommandResult `json:"commands"`
	Views    []ViewResult    `json:"views"`
}

// CommandResult is what Decide produced (personal data revealed), or its
// refusal. Error is the harness failing to ask, not the domain's answer.
type CommandResult struct {
	ID       string     `json:"id"`
	Produced []Produced `json:"produced,omitempty"`
	Rejected bool       `json:"rejected,omitempty"`
	Message  string     `json:"message,omitempty"`
	Error    string     `json:"error,omitempty"`
}

// Produced is one event Decide would append.
type Produced struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// ViewResult is every run's collection's rows, keyed by the key field, with
// personal data revealed and PocketBase's own system fields left out.
type ViewResult struct {
	ID    string                               `json:"id"`
	Rows  map[string]map[string]map[string]any `json:"rows,omitempty"`
	Error string                               `json:"error,omitempty"`
}
