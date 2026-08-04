// Package scaffold generates a working vertical slice — a JS decider and a
// JS projection — from a small description of a domain.
//
// It is deliberately built around an intermediate model rather than around
// the wizard that first needed it. There are two front-ends to the same
// generator: the dashboard's scaffolder, which collects the model from a
// form, and (M14) the eventmodelschema importer, which maps a JSON document
// onto it. Generating from the model, not from either input format, is what
// keeps those two from growing separate opinions about what a decider looks
// like.
//
// What it produces is a starting point, not a finished domain: the decider
// records what happened and refuses the obvious contradictions, and the
// projection maintains one read model. The interesting invariants are the
// author's job, which is why every generated file is meant to be dry-run and
// edited rather than shipped as-is.
package scaffold

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// identifier matches names that are safe in a file name, a JS identifier,
// a collection name and an index name at once — the intersection of every
// place these strings end up.
var identifier = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

// Domain is the intermediate model: what a slice is, independent of whether
// a form or a schema document described it.
type Domain struct {
	// Aggregate names the write-side stream family (lower camel by
	// convention: "order", "supportTicket").
	Aggregate string `json:"aggregate"`
	// Commands are the intents accepted. Each produces exactly one event in
	// the generated code — the common case, and the one worth generating.
	Commands []Command `json:"commands"`
	// ReadModel is the collection the projection maintains. Optional: a
	// slice may be write-only at first.
	ReadModel *ReadModel `json:"readModel,omitempty"`
}

// Command is one intent and the event it records.
type Command struct {
	Name string `json:"name"`
	// Event is the event this command appends. Defaults to the command name
	// in past tense where that is mechanical, but is explicit here because
	// English is not.
	Event string `json:"event"`
	// Fields are the payload fields, carried onto the event unchanged.
	Fields []Field `json:"fields,omitempty"`
	// Once marks a command that may only succeed on a fresh stream (the
	// "create" of the slice); the generated decider refuses a repeat.
	Once bool `json:"once,omitempty"`
	// RequiresExisting marks a command that needs the aggregate to exist
	// already — everything after the create, usually.
	RequiresExisting bool `json:"requiresExisting,omitempty"`
}

// Field is one payload/schema field.
type Field struct {
	Name string `json:"name"`
	// Type is a //@schema type: text, number, bool, date or json.
	Type string `json:"type"`
}

// ReadModel is the projection's target collection.
type ReadModel struct {
	Collection string `json:"collection"`
	// Key is the field carrying the row identity — the aggregate id, in the
	// generated projection, so a row per stream.
	Key string `json:"key"`
	// Fields are the columns. The key field is added automatically if it is
	// not listed.
	Fields []Field `json:"fields,omitempty"`
	// On lists the events that update the row. Empty means every event the
	// commands produce.
	On []string `json:"on,omitempty"`
}

// File is one generated source file.
type File struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	// Kind is what the file declares, matching functions.Kind values, so a
	// caller can pick the right dry-run mode without re-parsing.
	Kind string `json:"kind"`
}

var schemaTypes = map[string]bool{
	"text": true, "number": true, "bool": true, "date": true, "json": true,
}

// Validate reports every problem with the model at once. Generating from a
// model that cannot work produces files that fail at save time with an error
// about the generated code, which points at the generator rather than at the
// description that caused it.
func (d Domain) Validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if !identifier.MatchString(d.Aggregate) {
		add("aggregate name %q must be a letter followed by letters, digits or underscores", d.Aggregate)
	}
	if len(d.Commands) == 0 {
		add("declare at least one command: an aggregate that accepts nothing can never produce an event")
	}

	seenCmd, seenEvent := map[string]bool{}, map[string]bool{}
	creates := 0
	for i, c := range d.Commands {
		if !identifier.MatchString(c.Name) {
			add("command %d: name %q must be a letter followed by letters, digits or underscores", i+1, c.Name)
		}
		if !identifier.MatchString(c.Event) {
			add("command %q: event name %q must be a letter followed by letters, digits or underscores", c.Name, c.Event)
		}
		if seenCmd[c.Name] {
			add("command %q is declared twice", c.Name)
		}
		if seenEvent[c.Event] {
			add("event %q is produced by more than one command; give each command its own event", c.Event)
		}
		seenCmd[c.Name], seenEvent[c.Event] = true, true
		if c.Once {
			creates++
		}
		if c.Once && c.RequiresExisting {
			add("command %q cannot be both the create and require an existing aggregate", c.Name)
		}
		for _, f := range c.Fields {
			if !identifier.MatchString(f.Name) {
				add("command %q: field name %q is not a valid identifier", c.Name, f.Name)
			}
			if !schemaTypes[f.Type] {
				add("command %q: field %q has type %q; use text, number, bool, date or json", c.Name, f.Name, f.Type)
			}
		}
	}
	if creates > 1 {
		add("more than one command is marked as the create; a stream has one beginning")
	}

	if rm := d.ReadModel; rm != nil {
		if !identifier.MatchString(rm.Collection) {
			add("read model collection %q must be a letter followed by letters, digits or underscores", rm.Collection)
		}
		if !identifier.MatchString(rm.Key) {
			add("read model key %q is not a valid field name", rm.Key)
		}
		for _, f := range rm.Fields {
			if !identifier.MatchString(f.Name) {
				add("read model field name %q is not a valid identifier", f.Name)
			}
			if !schemaTypes[f.Type] {
				add("read model field %q has type %q; use text, number, bool, date or json", f.Name, f.Type)
			}
		}
		for _, on := range rm.On {
			if !seenEvent[on] {
				add("read model listens for %q, which no command produces", on)
			}
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("scaffold: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Events returns every event the commands produce, in declaration order.
func (d Domain) Events() []string {
	out := make([]string, 0, len(d.Commands))
	for _, c := range d.Commands {
		out = append(out, c.Event)
	}
	return out
}

// Generate produces the slice's files. The caller saves them through the
// ordinary function-file API, so they go through the same load check, the
// same dry run and the same maintenance barrier as hand-written code —
// generated code gets no shortcut.
func (d Domain) Generate() ([]File, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	files := []File{{
		Name:   d.Aggregate + ".js",
		Source: d.decider(),
		Kind:   "decider",
	}}
	if d.ReadModel != nil {
		files = append(files, File{
			Name:   d.ReadModel.Collection + ".js",
			Source: d.projection(),
			Kind:   "projection",
		})
	}
	return files, nil
}

func (d Domain) decider() string {
	var b strings.Builder
	events := d.Events()

	fmt.Fprintf(&b, "//@trigger decider %s\n", d.Aggregate)
	fmt.Fprintf(&b, "//@handles %s\n", strings.Join(events, " "))
	fmt.Fprintf(&b, "//@commands %s\n", strings.Join(commandNames(d.Commands), " "))
	b.WriteString("//\n")
	fmt.Fprintf(&b, "// %s — generated slice. The shape is right; the RULES are yours:\n", d.Aggregate)
	b.WriteString("// the checks below are the ones that can be derived from a description\n")
	b.WriteString("// (does it exist, does it exist already). Everything that makes this\n")
	b.WriteString("// domain rather than any other goes in decide().\n")
	b.WriteString("//\n")
	b.WriteString("// Dry-run this against real history before saving.\n\n")

	b.WriteString("function initialState() {\n  return { exists: false };\n}\n\n")

	b.WriteString("function decide(command, state) {\n")
	b.WriteString("  switch (command.name) {\n")
	for _, c := range d.Commands {
		fmt.Fprintf(&b, "    case '%s':\n", c.Name)
		if c.Once {
			fmt.Fprintf(&b, "      if (state.exists) { throw new Error('%s already exists'); }\n", d.Aggregate)
		}
		if c.RequiresExisting {
			fmt.Fprintf(&b, "      if (!state.exists) { throw new Error('%s does not exist'); }\n", d.Aggregate)
		}
		fmt.Fprintf(&b, "      return [{ type: '%s', data: %s }];\n", c.Event, payloadLiteral(c.Fields))
	}
	b.WriteString("    default:\n")
	b.WriteString("      throw new Error('unknown command: ' + command.name);\n")
	b.WriteString("  }\n}\n\n")

	b.WriteString("// Evolve folds an event into state. It must never throw and never\n")
	b.WriteString("// reject: it replays history, including events written by code older\n")
	b.WriteString("// than this file.\n")
	b.WriteString("function evolve(state, event) {\n")
	b.WriteString("  switch (event.type) {\n")
	for _, c := range d.Commands {
		fmt.Fprintf(&b, "    case '%s':\n", c.Event)
		fmt.Fprintf(&b, "      return Object.assign({}, state, event.data, { exists: true });\n")
	}
	b.WriteString("    default:\n      return state;\n")
	b.WriteString("  }\n}\n")
	return b.String()
}

func (d Domain) projection() string {
	rm := d.ReadModel
	on := rm.On
	if len(on) == 0 {
		on = d.Events()
	}

	// the key column always exists, whether or not it was listed
	fields := make([]Field, 0, len(rm.Fields)+1)
	fields = append(fields, Field{Name: rm.Key, Type: "text"})
	for _, f := range rm.Fields {
		if f.Name != rm.Key {
			fields = append(fields, f)
		}
	}
	columns := make([]string, 0, len(fields))
	for _, f := range fields {
		columns = append(columns, f.Name+":"+f.Type)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "//@trigger projection %s on %s\n", rm.Collection, strings.Join(on, " "))
	fmt.Fprintf(&b, "//@schema %s %s\n", rm.Collection, strings.Join(columns, " "))
	fmt.Fprintf(&b, "//@key %s\n", rm.Key)
	b.WriteString("//\n")
	fmt.Fprintf(&b, "// One row per %s stream, keyed by the aggregate id.\n", d.Aggregate)
	b.WriteString("//\n")
	b.WriteString("// project() returns ROW OPS — {upsert: {key, fields}} or {delete: key}.\n")
	b.WriteString("// A plain object is not an op and is discarded, so a projection that\n")
	b.WriteString("// returns rows directly writes nothing. Upserts merge, so each event\n")
	b.WriteString("// contributes the fields it knows about.\n\n")

	b.WriteString("function project(event) {\n")
	fmt.Fprintf(&b, "  const fields = { %s: event.aggregateId };\n", rm.Key)
	b.WriteString("  for (const name of Object.keys(event.data || {})) {\n")
	fmt.Fprintf(&b, "    if (%s.includes(name)) { fields[name] = event.data[name]; }\n", columnNamesLiteral(fields, rm.Key))
	b.WriteString("  }\n")
	fmt.Fprintf(&b, "  return [{ upsert: { key: event.aggregateId, fields: fields } }];\n")
	b.WriteString("}\n")
	return b.String()
}

func commandNames(cmds []Command) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c.Name)
	}
	return out
}

// payloadLiteral builds the object literal a command copies onto its event.
// Fields are taken from the payload by name, so an absent one lands as
// undefined rather than silently as something else.
func payloadLiteral(fields []Field) string {
	if len(fields) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, fmt.Sprintf("%s: command.payload.%s", f.Name, f.Name))
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// columnNamesLiteral renders the copyable columns as a JS array literal,
// excluding the key (which is set from the aggregate id, not the payload).
func columnNamesLiteral(fields []Field, key string) string {
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		if f.Name != key {
			names = append(names, "'"+f.Name+"'")
		}
	}
	sort.Strings(names)
	return "[" + strings.Join(names, ", ") + "]"
}
