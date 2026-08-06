// Package scaffold generates a working vertical slice — a JS decider, JS
// projections and JS reactors — from a small description of a domain.
//
// It is deliberately built around an intermediate model rather than around
// the wizard that first needed it. There are two front-ends to the same
// generator: the dashboard's scaffolder, which collects the model from a
// form, and the eventmodelschema importer, which maps a JSON document onto
// it. Generating from the model, not from either input format, is what keeps
// those two from growing separate opinions about what a decider looks like.
//
// THE MODEL RECORDS WHAT CAN RESULT, NOT HOW THE RESULT IS CHOSEN. A command
// may list several events; nothing here says which one applies when, because
// that is business logic and it lives in decide(), which the author edits.
// An earlier draft carried per-branch conditions and was rejected as
// over-constraining — over-specifying the model narrows what it can describe
// for no gain the generator can use.
//
// What it produces is a starting point, not a finished domain: the decider
// records what happened and refuses the obvious contradictions, and the
// projections maintain read models. The interesting invariants are the
// author's job, which is why every generated file is meant to be dry-run and
// edited rather than shipped as-is.
package scaffold

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
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
	// Commands are the intents accepted.
	Commands []Command `json:"commands"`
	// ReadModels are the collections projections maintain. A slice may have
	// none (write-only at first) or several — a schema document routinely
	// declares several read models over one aggregate's events.
	ReadModels []ReadModel `json:"readModels,omitempty"`
	// Reactors map this aggregate's events to commands on another one.
	Reactors []Reactor `json:"reactors,omitempty"`
}

// Command is one intent and the events it may record.
type Command struct {
	Name string `json:"name"`
	// Events are the events this command MAY append. More than one covers
	// both shapes a document can describe, which are indistinguishable in
	// the model on purpose:
	//
	//   conjunction — all of them, together (OrderConfirmed + StockReserved)
	//   disjunction — one of them, by state (PaymentAccepted xor Refused)
	//
	// The generator emits the first as a runnable default and says in a
	// comment that the choice is the author's; the count is reported as a
	// warning so the unfinished rule is named rather than buried.
	Events []Event `json:"events"`
	// Fields are the command's payload fields.
	Fields []Field `json:"fields,omitempty"`
	// Once marks a command that may only succeed on a fresh stream (the
	// "create" of the slice); the generated decider refuses a repeat.
	Once bool `json:"once,omitempty"`
	// RequiresExisting marks a command that needs the aggregate to exist
	// already — everything after the create, usually.
	RequiresExisting bool `json:"requiresExisting,omitempty"`
}

// Event is one event a command may record.
//
// Fields are NEVER inherited implicitly from the command. An event either
// declares its payload or declares itself empty, because "this event carries
// nothing" and "nobody has said what this event carries" must not look
// alike — that ambiguity is the shape of defect this project keeps paying
// for. A front-end that wants the command's fields on the event copies them
// when it builds the model.
type Event struct {
	Name   string  `json:"name"`
	Fields []Field `json:"fields,omitempty"`
	// NoFields declares "this event genuinely carries no payload". Without
	// it, an event with no fields is reported as UNSPECIFIED.
	NoFields bool `json:"noFields,omitempty"`
}

// Field is one payload/schema field.
type Field struct {
	Name string `json:"name"`
	// Type is a //@schema type: text, number, bool, date or json.
	Type string `json:"type"`
}

// ReadModel is a projection's target collection.
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

// Reactor maps events to a command on another aggregate — the automation
// shape. The target id is derived from the source event so that delivery
// being at-least-once cannot produce the reaction twice.
type Reactor struct {
	// Name is the file basename and the durable checkpoint suffix.
	Name string `json:"name"`
	// On lists the event types that trigger it.
	On []string `json:"on"`
	// Aggregate and Command name what it dispatches.
	Aggregate string `json:"aggregate"`
	Command   string `json:"command"`
	// IDPrefix prefixes the source aggregate id to build the target id
	// (e.g. "fulfill-" → "fulfill-<orderId>"). Deterministic on purpose:
	// a replay then hits the target's own "already exists" rule.
	IDPrefix string `json:"idPrefix,omitempty"`
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
//
// It is a HARD gate, shared by the wizard and the importer, so it reports
// only what makes generation impossible. Things that are merely unfinished —
// an event whose payload nobody specified, a command needing an outcome
// rule — are Warnings, because a schema document routinely carries neither
// and refusing those would make real documents unimportable.
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
		if seenCmd[c.Name] {
			add("command %q is declared twice", c.Name)
		}
		seenCmd[c.Name] = true
		if len(c.Events) == 0 {
			add("command %q records no event; a command that changes nothing is a query", c.Name)
		}
		for _, e := range c.Events {
			if !identifier.MatchString(e.Name) {
				add("command %q: event name %q must be a letter followed by letters, digits or underscores", c.Name, e.Name)
			}
			seenEvent[e.Name] = true
			for _, f := range e.Fields {
				if !identifier.MatchString(f.Name) {
					add("command %q, event %q: field name %q is not a valid identifier", c.Name, e.Name, f.Name)
				}
				if !schemaTypes[f.Type] {
					add("command %q, event %q: field %q has type %q; use text, number, bool, date or json",
						c.Name, e.Name, f.Name, f.Type)
				}
			}
		}
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

	// NOTE: an event produced by more than one command is NOT an error. A
	// schema document may legitimately reference one event id from several
	// slices, and refusing that would make a well-formed document
	// unimportable for what would look like a generator bug.

	seenCollection := map[string]bool{}
	for _, rm := range d.ReadModels {
		if !identifier.MatchString(rm.Collection) {
			add("read model collection %q must be a letter followed by letters, digits or underscores", rm.Collection)
		}
		if seenCollection[rm.Collection] {
			add("read model collection %q is declared twice", rm.Collection)
		}
		seenCollection[rm.Collection] = true
		if !identifier.MatchString(rm.Key) {
			add("read model %q: key %q is not a valid field name", rm.Collection, rm.Key)
		}
		for _, f := range rm.Fields {
			if !identifier.MatchString(f.Name) {
				add("read model %q: field name %q is not a valid identifier", rm.Collection, f.Name)
			}
			if !schemaTypes[f.Type] {
				add("read model %q: field %q has type %q; use text, number, bool, date or json", rm.Collection, f.Name, f.Type)
			}
		}
		// NOTE: a read model listening for an event no command here produces
		// is NOT an error. The schema gives read models no aggregate at all,
		// deliberately — they are cross-cutting, and one routinely folds
		// events from several aggregates. Refusing that would make a
		// well-formed document unimportable. It IS reported by Warnings,
		// because it is also what a typo looks like.
	}

	seenReactor := map[string]bool{}
	for _, r := range d.Reactors {
		if !identifier.MatchString(r.Name) {
			add("reactor name %q must be a letter followed by letters, digits or underscores", r.Name)
		}
		if seenReactor[r.Name] {
			add("reactor %q is declared twice", r.Name)
		}
		seenReactor[r.Name] = true
		if len(r.On) == 0 {
			add("reactor %q declares no trigger events, so it can never fire", r.Name)
		}
		if !identifier.MatchString(r.Aggregate) {
			add("reactor %q: target aggregate %q is not a valid name", r.Name, r.Aggregate)
		}
		if !identifier.MatchString(r.Command) {
			add("reactor %q: target command %q is not a valid name", r.Name, r.Command)
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("scaffold: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Warnings reports what is UNFINISHED rather than broken — the things a
// generated slice will run with but an author still has to decide.
//
// They are warnings, not errors, because Validate is a hard gate the wizard
// shares and because a schema document routinely specifies neither payloads
// nor outcome rules; refusing those would make real documents unimportable.
// They are reported at all because burying an unfinished rule in a generated
// comment is how it stays unfinished.
func (d Domain) Warnings() []string {
	var out []string
	for _, c := range d.Commands {
		for _, e := range c.Events {
			// "carries nothing" and "nobody said what it carries" must not
			// look alike — an explicit NoFields is what tells them apart
			if len(e.Fields) == 0 && !e.NoFields {
				out = append(out, fmt.Sprintf(
					"event %q of command %q does not say what its payload is: declare fields, or mark it as carrying none",
					e.Name, c.Name))
			}
		}
		if len(c.Events) > 1 {
			names := make([]string, 0, len(c.Events))
			for _, e := range c.Events {
				names = append(names, e.Name)
			}
			out = append(out, fmt.Sprintf(
				"command %q can result in %d different events (%s) and needs an outcome rule written in decide(); the generated code returns the first",
				c.Name, len(c.Events), strings.Join(names, ", ")))
		}
	}

	// a projection over events this aggregate does not produce is legitimate
	// (read models are cross-cutting) and is also what a typo looks like, so
	// it is named rather than refused or ignored
	produced := map[string]bool{}
	for _, n := range d.Events() {
		produced[n] = true
	}
	for _, rm := range d.ReadModels {
		for _, on := range rm.On {
			if !produced[on] {
				out = append(out, fmt.Sprintf(
					"read model %q listens for %q, which no command of %q produces: intended if it folds another aggregate's events, a typo otherwise",
					rm.Collection, on, d.Aggregate))
			}
		}
	}
	return out
}

// Events returns every event the commands may produce, flattened across
// commands in declaration order and de-duplicated. It feeds //@handles and a
// projection's default trigger list, where flattening is all that is wanted.
func (d Domain) Events() []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range d.Commands {
		for _, e := range c.Events {
			if seen[e.Name] {
				continue
			}
			seen[e.Name] = true
			out = append(out, e.Name)
		}
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
	for _, rm := range d.ReadModels {
		files = append(files, File{
			Name:   rm.Collection + ".js",
			Source: d.projection(rm),
			Kind:   "projection",
		})
	}
	for _, r := range d.Reactors {
		files = append(files, File{
			Name:   r.Name + ".js",
			Source: d.reactor(r),
			Kind:   "reactor",
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
		// More than one possible event means the description said WHAT can
		// result, not WHICH applies. The generator must not invent the rule:
		// it returns the first and says so, and Warnings() counts it so the
		// gap is named somewhere an operator will actually look.
		if len(c.Events) > 1 {
			names := make([]string, 0, len(c.Events))
			for _, e := range c.Events {
				names = append(names, e.Name)
			}
			b.WriteString("      // THIS COMMAND CAN RESULT IN MORE THAN ONE EVENT:\n")
			fmt.Fprintf(&b, "      //   %s\n", strings.Join(names, ", "))
			b.WriteString("      // Which one applies is the domain rule, and only you can write it.\n")
			b.WriteString("      // The line below returns the first as a placeholder so the slice\n")
			b.WriteString("      // runs; replace it with the real decision.\n")
		}
		e := c.Events[0]
		fmt.Fprintf(&b, "      return [{ type: '%s', data: %s }];\n", e.Name, payloadLiteral(e.Fields))
	}
	b.WriteString("    default:\n")
	b.WriteString("      throw new Error('unknown command: ' + command.name);\n")
	b.WriteString("  }\n}\n\n")

	b.WriteString("// Evolve folds an event into state. It must never throw and never\n")
	b.WriteString("// reject: it replays history, including events written by code older\n")
	b.WriteString("// than this file.\n")
	b.WriteString("function evolve(state, event) {\n")
	b.WriteString("  switch (event.type) {\n")
	for _, name := range events {
		fmt.Fprintf(&b, "    case '%s':\n", name)
		b.WriteString("      return Object.assign({}, state, event.data, { exists: true });\n")
	}
	b.WriteString("    default:\n      return state;\n")
	b.WriteString("  }\n}\n")
	return b.String()
}

func (d Domain) projection(rm ReadModel) string {
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

func (d Domain) reactor(r Reactor) string {
	var b strings.Builder
	fmt.Fprintf(&b, "//@trigger react %s\n", strings.Join(r.On, " "))
	fmt.Fprintf(&b, "//@dispatches %s/%s\n", r.Aggregate, r.Command)
	b.WriteString("//\n")
	fmt.Fprintf(&b, "// %s — generated automation: %s's events become %s commands\n",
		r.Name, d.Aggregate, r.Aggregate)
	fmt.Fprintf(&b, "// on %s.\n", r.Aggregate)
	b.WriteString("//\n")
	b.WriteString("// react() RETURNS dispatch descriptors; the host sends them through the\n")
	b.WriteString("// decider registry, so a reaction is a COMMAND that can be refused —\n")
	b.WriteString("// never a direct append.\n")
	b.WriteString("//\n")
	b.WriteString("// Delivery is at-least-once. The target id below is derived from the\n")
	b.WriteString("// source event, so a replay hits the target's own 'already exists' rule\n")
	b.WriteString("// instead of dispatching twice. Keep it deterministic.\n\n")

	b.WriteString("function react(event) {\n")
	b.WriteString("  return [{\n")
	fmt.Fprintf(&b, "    aggregate: '%s',\n", r.Aggregate)
	fmt.Fprintf(&b, "    id: '%s' + event.aggregateId,\n", r.IDPrefix)
	fmt.Fprintf(&b, "    command: '%s',\n", r.Command)
	b.WriteString("    payload: Object.assign({}, event.data)\n")
	b.WriteString("  }];\n")
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

// SanitizeName renders an arbitrary string as a name this package accepts:
// a letter followed by letters, digits or underscores.
//
// It exists for the importer, which receives names from a document that has
// its own, wider, notion of what a name may contain. Sanitizing rather than
// refusing is right here because the alternative is rejecting a valid
// document over a hyphen — but the result is checked by Validate all the
// same, so a string with nothing usable in it still fails loudly rather than
// becoming a silently odd identifier.
func SanitizeName(s string) string {
	var b strings.Builder
	upperNext := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r):
			if upperNext && b.Len() > 0 {
				r = unicode.ToUpper(r)
			}
			b.WriteRune(r)
			upperNext = false
		case unicode.IsDigit(r) || r == '_':
			if b.Len() == 0 {
				continue // an identifier cannot start with a digit
			}
			b.WriteRune(r)
			upperNext = false
		default:
			// a separator joins the next word rather than vanishing, so
			// "order-placed" becomes "orderPlaced" and not "orderplaced"
			upperNext = true
		}
	}
	return b.String()
}
