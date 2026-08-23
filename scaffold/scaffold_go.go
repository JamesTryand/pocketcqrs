package scaffold

import (
	"fmt"
	"go/format"
	"sort"
	"strings"
	"unicode"
)

// GoFile is one generated Go source file. Its own shape, not File{Kind:
// ...}: Kind selects a JS dry-run mode and has no Go counterpart, so
// reusing it would leave a meaningless field on Go output.
type GoFile struct {
	Name   string
	Source string
}

// namedField is one entry in the union of every field this domain's events
// declare, keyed by name — see collectEventFields.
type namedField struct {
	Name   string
	GoType string
}

// goType maps a //@schema type onto the Go type a generated struct field
// gets. The five //@schema types are themselves already a fold of ten
// eventmodelschema types (docs/schema.md), so this is the second half of
// that same fold, documented at the same place this table is described:
// docs/go-guide.md's JS→Go conversion table.
func goType(schemaType string) string {
	switch schemaType {
	case "text":
		return "string"
	case "number":
		return "float64"
	case "bool":
		return "bool"
	case "date":
		return "time.Time"
	case "json":
		return "json.RawMessage"
	default:
		// unreachable: Validate rejects any type not in schemaTypes before
		// generation ever runs.
		return "string"
	}
}

// collectEventFields unions every field every event this domain produces
// declares, in first-declared order, resolving each name to one Go type.
// The command's own Fields are deliberately excluded — same as the JS
// generator, which builds a command's payload literal from the event's own
// Fields, never the command's (see TestEventFieldsAreNotInheritedFromTheCommand).
//
// A field name declared with more than one //@schema type across different
// events is a real model ambiguity: the first type seen wins, silently, in
// the JS generator's untyped world, but a Go struct needs one field with one
// type used consistently everywhere that name appears (the State struct,
// every event's own unmarshal-target struct, every command's payload
// struct) — otherwise a later event's own type would fail to compile
// against State's. So the conflict is resolved the same way (first wins)
// and named, via the returned warning, rather than left to surface as a
// compile error somewhere else in the generated file.
func (d Domain) collectEventFields() ([]namedField, []string) {
	var order []string
	types := map[string]string{}
	firstEvent := map[string]string{}
	var warnings []string

	for _, c := range d.Commands {
		for _, e := range c.Events {
			for _, f := range e.Fields {
				existing, seen := types[f.Name]
				if !seen {
					types[f.Name] = f.Type
					firstEvent[f.Name] = e.Name
					order = append(order, f.Name)
					continue
				}
				if existing != f.Type {
					warnings = append(warnings, fmt.Sprintf(
						"field %q is declared as %q by event %q but %q by event %q; "+
							"the Go generator uses %q everywhere and keeps the first type it saw",
						f.Name, existing, firstEvent[f.Name], f.Type, e.Name, existing))
				}
			}
		}
	}

	fields := make([]namedField, 0, len(order))
	for _, name := range order {
		fields = append(fields, namedField{Name: name, GoType: goType(types[name])})
	}
	return fields, warnings
}

// fieldTypeConflictWarnings extends Warnings() with the Go-specific
// ambiguity collectEventFields resolves. It is folded into Warnings()
// itself (rather than a separate GoWarnings) because the underlying
// ambiguity is a property of the model, not of which language is
// generated — printing it even for a JS-only import is harmless, and it
// would be surprising for the same model to warn differently depending on
// which generator happens to run against it.
func (d Domain) fieldTypeConflictWarnings() []string {
	_, warnings := d.collectEventFields()
	return warnings
}

// ExportName renders name as an exported Go identifier: SanitizeName's
// camelCase output with its first rune upper-cased. Model field/event/
// command names are already validated identifiers (Validate's regex), so
// this only ever needs to fix casing, never punctuation. Exported for the
// same reason as GoPackageName: a caller printing a suggested constructor
// call (schema_cmd.go's printGoWiringSuggestions) must name the exact
// function GenerateGo's files declare, not a re-derived copy of the same
// transform that could drift from it.
func ExportName(name string) string {
	if name == "" {
		return name
	}
	r := []rune(name)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// GoPackageName lower-cases only the first rune of aggregate for the
// package clause GenerateGo's files declare. d.Aggregate itself is left
// untouched everywhere else (the //@trigger-equivalent registration
// string, file names) — Go package names are conventionally all-lowercase,
// but the aggregate's own identity as recorded in the model is not a Go
// concern and must not silently change because of one. Exported so a
// caller printing a suggested import/registration line (see this repo's
// own schema_cmd.go) names the same package GenerateGo's files actually
// declare, rather than re-deriving the same one-line transform and risking
// drift.
func GoPackageName(aggregate string) string {
	r := []rune(aggregate)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

func usesGoType(fields []namedField, want string) bool {
	for _, f := range fields {
		if f.GoType == want {
			return true
		}
	}
	return false
}

// eventFields returns the Fields the FIRST event named name declares
// (across every command), for building that event's own unmarshal-target
// struct in Evolve. Events sharing a name are expected to share a shape —
// the model doesn't allow declaring the same event name twice with
// different fields on purpose, so "first" and "only" coincide in practice.
func (d Domain) eventFields(name string) []Field {
	for _, c := range d.Commands {
		for _, e := range c.Events {
			if e.Name == name {
				return e.Fields
			}
		}
	}
	return nil
}

// formatGo runs src through go/format, turning a codegen mistake into an
// error the caller sees immediately rather than a .go file that fails to
// build only once someone tries to compile it.
func formatGo(src string) (string, error) {
	out, err := format.Source([]byte(src))
	if err != nil {
		return "", fmt.Errorf("scaffold: generated Go source is invalid: %w\n---\n%s", err, src)
	}
	return string(out), nil
}

// GenerateGo produces the slice's files as compilable Go source: a decider
// beside the existing JS generator's decider(), one projection per read
// model, one reactor per reactor — same model, same Validate()/Warnings()
// gate, second output language. THE GENERATED CODE IS A STARTING POINT, NOT
// A TRANSLATION: there is deliberately no JS→Go transpiler (see
// docs/go-guide.md's "A word of caution on scope") — a live JS file's
// actual decide()/evolve() rules are the author's to re-express in Go by
// hand, using the JS as reference, exactly as the JS scaffolder's own
// output is a starting point whose rules the author writes.
//
// Every file shares one package, named after the aggregate (lower-cased
// only for the package clause — see GoPackageName): a vertical slice is
// naturally one unit, and this repo's own aggregates/projections/reactors
// split is its own tutorial's tier-per-package layout, not a convention to
// impose on a downstream consumer's repo (whose own layout is deliberately
// undocumented elsewhere — see FAULTS-AND-WORK.md D-2).
func (d Domain) GenerateGo() ([]GoFile, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}

	pkg := GoPackageName(d.Aggregate)
	fields, _ := d.collectEventFields()

	deciderSrc, err := formatGo(d.deciderGo(pkg, fields))
	if err != nil {
		return nil, err
	}
	files := []GoFile{{Name: d.Aggregate + ".go", Source: deciderSrc}}

	for _, rm := range d.ReadModels {
		src, err := formatGo(d.projectionGo(pkg, rm))
		if err != nil {
			return nil, err
		}
		files = append(files, GoFile{Name: rm.Collection + ".go", Source: src})
	}

	for _, r := range d.Reactors {
		src, err := formatGo(d.reactorGo(pkg, r))
		if err != nil {
			return nil, err
		}
		files = append(files, GoFile{Name: r.Name + ".go", Source: src})
	}

	return files, nil
}

func (d Domain) deciderGo(pkg string, fields []namedField) string {
	events := d.Events()

	// Resolved per collectEventFields' first-wins rule -- used for EVERY
	// struct field emitted below (State, each command's payload struct,
	// each event's Evolve data struct), never a field's own locally
	// declared type. That is what makes a field-type conflict a warning
	// instead of a compile error: whichever event's own field disagrees
	// with the resolved type still gets unmarshaled/assigned using the
	// resolved one everywhere it's named.
	resolved := make(map[string]string, len(fields))
	for _, f := range fields {
		resolved[f.Name] = f.GoType
	}
	fieldGoType := func(name, schemaType string) string {
		if t, ok := resolved[name]; ok {
			return t
		}
		return goType(schemaType)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	b.WriteString("import (\n")
	b.WriteString("\t\"encoding/json\"\n")
	b.WriteString("\t\"fmt\"\n")
	if usesGoType(fields, "time.Time") {
		b.WriteString("\t\"time\"\n")
	}
	b.WriteString("\n")
	b.WriteString("\t\"github.com/jamestryand/pocketcqrs/decider\"\n")
	b.WriteString("\t\"github.com/jamestryand/pocketcqrs/events\"\n")
	b.WriteString(")\n\n")

	b.WriteString("// Event types this aggregate produces.\n")
	b.WriteString("const (\n")
	for _, name := range events {
		fmt.Fprintf(&b, "\t%s = %q\n", name, name)
	}
	b.WriteString(")\n\n")

	fmt.Fprintf(&b, "// State is %s's generated read side for Decide/Evolve. THE SHAPE IS\n", d.Aggregate)
	b.WriteString("// RIGHT, THE RULES ARE YOURS: fields come straight from the declared event\n")
	b.WriteString("// payloads, unioned across every event this aggregate produces. Dry-run\n")
	b.WriteString("// and test this against real history before relying on it.\n")
	b.WriteString("type State struct {\n")
	b.WriteString("\tExists bool\n")
	for _, f := range fields {
		fmt.Fprintf(&b, "\t%s %s `json:%q`\n", ExportName(f.Name), f.GoType, f.Name)
	}
	b.WriteString("}\n\n")

	ctor := ExportName(d.Aggregate)
	fmt.Fprintf(&b, "// %s builds the generated decider. Register it at bootstrap, next to\n", ctor)
	b.WriteString("// this project's own domains, not inside example wiring:\n")
	fmt.Fprintf(&b, "//\n//\tdecider.Register(registry, %q, %s())\n", d.Aggregate, ctor)
	fmt.Fprintf(&b, "func %s() *decider.Decider[State] {\n", ctor)
	b.WriteString("\treturn &decider.Decider[State]{\n")
	b.WriteString("\t\tInitialState: func() State { return State{} },\n")
	fmt.Fprintf(&b, "\t\tCommands:     []string{%s},\n", quotedList(commandNames(d.Commands)))
	b.WriteString("\t\tDecide: func(cmd decider.Command, s State) ([]events.NewEvent, error) {\n")
	b.WriteString("\t\t\tswitch cmd.Name {\n")
	for _, c := range d.Commands {
		fmt.Fprintf(&b, "\t\t\tcase %q:\n", c.Name)
		if c.Once {
			fmt.Fprintf(&b, "\t\t\t\tif s.Exists {\n\t\t\t\t\treturn nil, fmt.Errorf(%q)\n\t\t\t\t}\n", d.Aggregate+" already exists")
		}
		if c.RequiresExisting {
			fmt.Fprintf(&b, "\t\t\t\tif !s.Exists {\n\t\t\t\t\treturn nil, fmt.Errorf(%q)\n\t\t\t\t}\n", d.Aggregate+" does not exist")
		}
		if len(c.Events) > 1 {
			names := eventNames(c.Events)
			b.WriteString("\t\t\t\t// THIS COMMAND CAN RESULT IN MORE THAN ONE EVENT:\n")
			fmt.Fprintf(&b, "\t\t\t\t//   %s\n", strings.Join(names, ", "))
			b.WriteString("\t\t\t\t// Which one applies is the domain rule, and only you can write it.\n")
			b.WriteString("\t\t\t\t// The code below returns the first as a placeholder so the slice\n")
			b.WriteString("\t\t\t\t// runs; replace it with the real decision.\n")
		}
		e := c.Events[0]
		if len(e.Fields) == 0 {
			fmt.Fprintf(&b, "\t\t\t\treturn []events.NewEvent{{Type: %s, Data: json.RawMessage(\"{}\")}}, nil\n", e.Name)
		} else {
			b.WriteString("\t\t\t\tvar payload struct {\n")
			for _, f := range e.Fields {
				fmt.Fprintf(&b, "\t\t\t\t\t%s %s `json:%q`\n", ExportName(f.Name), fieldGoType(f.Name, f.Type), f.Name)
			}
			b.WriteString("\t\t\t\t}\n")
			b.WriteString("\t\t\t\tif err := json.Unmarshal(cmd.Payload, &payload); err != nil {\n\t\t\t\t\treturn nil, err\n\t\t\t\t}\n")
			b.WriteString("\t\t\t\tdata, err := json.Marshal(payload)\n\t\t\t\tif err != nil {\n\t\t\t\t\treturn nil, err\n\t\t\t\t}\n")
			fmt.Fprintf(&b, "\t\t\t\treturn []events.NewEvent{{Type: %s, Data: data}}, nil\n", e.Name)
		}
	}
	b.WriteString("\t\t\tdefault:\n")
	b.WriteString("\t\t\t\treturn nil, fmt.Errorf(\"unknown command: %s\", cmd.Name)\n")
	b.WriteString("\t\t\t}\n")
	b.WriteString("\t\t},\n")

	b.WriteString("\t\tEvolve: func(s State, ev events.Event) (State, error) {\n")
	b.WriteString("\t\t\tswitch ev.Type {\n")
	for _, name := range events {
		ef := d.eventFields(name)
		fmt.Fprintf(&b, "\t\t\tcase %s:\n", name)
		if len(ef) == 0 {
			b.WriteString("\t\t\t\ts.Exists = true\n")
			continue
		}
		b.WriteString("\t\t\t\tvar data struct {\n")
		for _, f := range ef {
			fmt.Fprintf(&b, "\t\t\t\t\t%s %s `json:%q`\n", ExportName(f.Name), fieldGoType(f.Name, f.Type), f.Name)
		}
		b.WriteString("\t\t\t\t}\n")
		b.WriteString("\t\t\t\tif err := json.Unmarshal(ev.Data, &data); err != nil {\n\t\t\t\t\treturn s, err\n\t\t\t\t}\n")
		for _, f := range ef {
			fmt.Fprintf(&b, "\t\t\t\ts.%s = data.%s\n", ExportName(f.Name), ExportName(f.Name))
		}
		b.WriteString("\t\t\t\ts.Exists = true\n")
	}
	b.WriteString("\t\t\t}\n")
	b.WriteString("\t\t\treturn s, nil\n")
	b.WriteString("\t\t},\n")
	b.WriteString("\t}\n}\n")
	return b.String()
}

func (d Domain) projectionGo(pkg string, rm ReadModel) string {
	on := rm.On
	if len(on) == 0 {
		on = d.Events()
	}

	columns := make([]string, 0, len(rm.Fields))
	for _, f := range rm.Fields {
		if f.Name != rm.Key {
			columns = append(columns, f.Name)
		}
	}
	sort.Strings(columns)

	typeName := ExportName(rm.Collection) + "Projection"
	ctor := "New" + ExportName(rm.Collection)

	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	b.WriteString("import (\n")
	b.WriteString("\t\"context\"\n")
	b.WriteString("\t\"database/sql\"\n")
	b.WriteString("\t\"encoding/json\"\n")
	b.WriteString("\t\"errors\"\n\n")
	b.WriteString("\t\"github.com/pocketbase/pocketbase/core\"\n\n")
	b.WriteString("\t\"github.com/jamestryand/pocketcqrs/events\"\n")
	b.WriteString("\t\"github.com/jamestryand/pocketcqrs/writeguard\"\n")
	b.WriteString(")\n\n")

	fmt.Fprintf(&b, "// %s projects %s events into the %q collection, one row per %s\n", ctor, d.Aggregate, rm.Collection, d.Aggregate)
	b.WriteString("// stream keyed by the aggregate id — a generic field-merge, the same shape\n")
	b.WriteString("// the JS scaffolder's project() produces. Port your own per-event rules\n")
	b.WriteString("// once they've settled; the collection itself becomes an ordinary\n")
	b.WriteString("// PocketBase migration (see migrations/1754200000_tasks_collection.go for\n")
	b.WriteString("// the shape) — this file does not create it.\n")
	fmt.Fprintf(&b, "func %s(app core.App) *%s { return &%s{app: app} }\n\n", ctor, typeName, typeName)
	fmt.Fprintf(&b, "type %s struct {\n\tapp core.App\n}\n\n", typeName)
	fmt.Fprintf(&b, "func (p *%s) Name() string { return %q }\n", typeName, rm.Collection)
	fmt.Fprintf(&b, "func (p *%s) Collections() []string { return []string{%q} }\n\n", typeName, rm.Collection)

	fmt.Fprintf(&b, "func (p *%s) Apply(ctx context.Context, ev events.Event) error {\n", typeName)
	b.WriteString("\tswitch ev.Type {\n")
	// String literals, not the aggregate's own event constants: On may
	// legitimately name another aggregate's events entirely (read models
	// are cross-cutting -- see Warnings' "intended if it folds another
	// aggregate's events" note), which this package's decider.go would not
	// have declared a constant for.
	fmt.Fprintf(&b, "\tcase %s:\n", quotedList(on))
	b.WriteString("\tdefault:\n\t\treturn nil\n\t}\n")
	b.WriteString("\tctx = writeguard.MarkInternal(ctx)\n\n")
	b.WriteString("\tvar data map[string]any\n")
	b.WriteString("\tif err := json.Unmarshal(ev.Data, &data); err != nil {\n\t\treturn err\n\t}\n\n")
	fmt.Fprintf(&b, "\trec, err := p.app.FindFirstRecordByData(%q, %q, ev.AggregateID)\n", rm.Collection, rm.Key)
	b.WriteString("\tif err != nil && !errors.Is(err, sql.ErrNoRows) {\n\t\treturn err\n\t}\n")
	b.WriteString("\tif rec == nil {\n")
	fmt.Fprintf(&b, "\t\tcol, err := p.app.FindCollectionByNameOrId(%q)\n", rm.Collection)
	b.WriteString("\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n")
	b.WriteString("\t\trec = core.NewRecord(col)\n")
	fmt.Fprintf(&b, "\t\trec.Set(%q, ev.AggregateID)\n", rm.Key)
	b.WriteString("\t}\n")
	if len(columns) > 0 {
		fmt.Fprintf(&b, "\tfor _, name := range []string{%s} {\n", quotedList(columns))
		b.WriteString("\t\tif v, ok := data[name]; ok {\n\t\t\trec.Set(name, v)\n\t\t}\n\t}\n")
	}
	b.WriteString("\treturn p.app.SaveWithContext(ctx, rec)\n")
	b.WriteString("}\n")
	return b.String()
}

func (d Domain) reactorGo(pkg string, r Reactor) string {
	typeName := lowerFirst(ExportName(r.Name)) + "Reactor"
	ctor := ExportName(r.Name)

	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	b.WriteString("import (\n")
	b.WriteString("\t\"github.com/jamestryand/pocketcqrs/decider\"\n")
	b.WriteString("\t\"github.com/jamestryand/pocketcqrs/events\"\n")
	b.WriteString("\t\"github.com/jamestryand/pocketcqrs/reactors\"\n")
	b.WriteString(")\n\n")

	fmt.Fprintf(&b, "// %s reacts to %s's events by dispatching %s/%s — generated\n", ctor, d.Aggregate, r.Aggregate, r.Command)
	b.WriteString("// automation matching the JS scaffolder's reactTo(): one reaction per\n")
	b.WriteString("// triggering event, its payload the causing event's own data untouched.\n")
	b.WriteString("// The target id is derived from the source event, so a replay hits the\n")
	b.WriteString("// target's own \"already exists\" rule instead of dispatching twice — keep\n")
	b.WriteString("// it deterministic if you change it.\n")
	fmt.Fprintf(&b, "func %s() reactors.Reactor { return %s{} }\n\n", ctor, typeName)
	fmt.Fprintf(&b, "type %s struct{}\n\n", typeName)
	fmt.Fprintf(&b, "func (%s) Name() string { return %q }\n\n", typeName, r.Name)
	fmt.Fprintf(&b, "func (%s) React(ev events.Event) []reactors.Reaction {\n", typeName)
	b.WriteString("\tswitch ev.Type {\n")
	fmt.Fprintf(&b, "\tcase %s:\n", quotedList(r.On))
	b.WriteString("\tdefault:\n\t\treturn nil\n\t}\n")
	b.WriteString("\treturn []reactors.Reaction{{\n")
	fmt.Fprintf(&b, "\t\tAggregate: %q,\n", r.Aggregate)
	fmt.Fprintf(&b, "\t\tID:        %q + ev.AggregateID,\n", r.IDPrefix)
	b.WriteString("\t\tCommand:   decider.Command{Name: ")
	fmt.Fprintf(&b, "%q, Payload: ev.Data},\n", r.Command)
	b.WriteString("\t}}\n")
	b.WriteString("}\n")
	return b.String()
}

func eventNames(evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Name)
	}
	return out
}

// quotedList renders names as a comma-separated list of Go string literals,
// e.g. []string{"a", "b"} -> `"a", "b"` — used both for []string{...}
// literals and for switch-case lists (a switch case is just a
// comma-separated expression list, so the same rendering serves both).
func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(quoted, ", ")
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}
