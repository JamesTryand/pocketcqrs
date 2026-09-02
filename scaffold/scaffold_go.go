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

// eventEndsStream mirrors eventFields for the EndsStream flag: true when the
// FIRST event named name is tagged as closing its aggregate's stream.
func (d Domain) eventEndsStream(name string) bool {
	for _, c := range d.Commands {
		for _, e := range c.Events {
			if e.Name == name {
				return e.EndsStream
			}
		}
	}
	return false
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
		// endsStream events (an unassign, a removal) reset Exists instead of
		// setting it -- otherwise the generic fold only ever sets state and
		// never retracts it, permanently blocking a later create-shaped
		// command on the same stream once it has ever fired once (Finding 3).
		existsAfter := "true"
		if d.eventEndsStream(name) {
			existsAfter = "false"
		}
		fmt.Fprintf(&b, "\t\t\tcase %s:\n", name)
		if len(ef) == 0 {
			fmt.Fprintf(&b, "\t\t\t\ts.Exists = %s\n", existsAfter)
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
		fmt.Fprintf(&b, "\t\t\t\ts.Exists = %s\n", existsAfter)
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

	derived, order := collectDerivedActions(rm.Fields, rm.Key)

	typeName := ExportName(rm.Collection) + "Projection"
	ctor := "New" + ExportName(rm.Collection)

	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\n", pkg)

	if len(derived) == 0 {
		writeProjectionImports(&b, true)
		writeProjectionHeader(&b, d, rm, ctor, typeName)
		writeProjectionPlainApply(&b, rm, on, typeName)
		writeProjectionScopes(&b, rm, typeName)
		return b.String()
	}

	// Finding 3: at least one field is a fold over named events (a toggle,
	// a count or a sum) rather than a same-named payload copy. Those
	// trigger events are pulled OUT of the generic merge entirely and
	// routed to dedicated per-kind helpers instead — an event that feeds a
	// derived field is treated as a derivation trigger only, never also
	// merged generically, which is the one simplification this generator
	// makes (see the codegen-handwrite-gaps write-up for the rationale).
	mergeEvents := subtractStrings(on, derived)
	plainColumns := plainColumnNames(rm.Fields, rm.Key)
	usesToggle, usesCount, usesSum := false, false, false
	usesGroupByCount, usesGroupBySum := false, false
	for _, acts := range derived {
		for _, a := range acts {
			switch {
			case a.isGroupBy && a.kind == DerivationCount:
				usesGroupByCount = true
			case a.isGroupBy && a.kind == DerivationSum:
				usesGroupBySum = true
			case a.kind == DerivationToggle:
				usesToggle = true
			case a.kind == DerivationCount:
				usesCount = true
			case a.kind == DerivationSum:
				usesSum = true
			}
		}
	}
	usesJSON := len(mergeEvents) > 0 || usesCount || usesSum || usesGroupByCount || usesGroupBySum

	writeProjectionImports(&b, usesJSON)
	writeProjectionHeader(&b, d, rm, ctor, typeName)

	fmt.Fprintf(&b, "func (p *%s) Apply(ctx context.Context, ev events.Event) error {\n", typeName)
	b.WriteString("\tswitch ev.Type {\n")
	// String literals, not the aggregate's own event constants: On may
	// legitimately name another aggregate's events entirely (read models
	// are cross-cutting -- see Warnings' "intended if it folds another
	// aggregate's events" note), which this package's decider.go would not
	// have declared a constant for.
	if len(mergeEvents) > 0 {
		fmt.Fprintf(&b, "\tcase %s:\n", quotedList(mergeEvents))
		b.WriteString("\t\treturn p.applyMerge(ctx, ev)\n")
	}
	for _, evName := range order {
		fmt.Fprintf(&b, "\tcase %q:\n", evName)
		acts := derived[evName]
		for i, a := range acts {
			call := a.call()
			if i == len(acts)-1 {
				fmt.Fprintf(&b, "\t\treturn %s\n", call)
			} else {
				fmt.Fprintf(&b, "\t\tif err := %s; err != nil {\n\t\t\treturn err\n\t\t}\n", call)
			}
		}
	}
	b.WriteString("\tdefault:\n\t\treturn nil\n\t}\n")
	b.WriteString("}\n\n")

	fmt.Fprintf(&b, "func (p *%s) getOrCreate(rowKey string) (*core.Record, error) {\n", typeName)
	fmt.Fprintf(&b, "\trec, err := p.app.FindFirstRecordByData(%q, %q, rowKey)\n", rm.Collection, rm.Key)
	b.WriteString("\tif err != nil && !errors.Is(err, sql.ErrNoRows) {\n\t\treturn nil, err\n\t}\n")
	b.WriteString("\tif rec != nil {\n\t\treturn rec, nil\n\t}\n")
	fmt.Fprintf(&b, "\tcol, err := p.app.FindCollectionByNameOrId(%q)\n", rm.Collection)
	b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
	b.WriteString("\trec = core.NewRecord(col)\n")
	fmt.Fprintf(&b, "\trec.Set(%q, rowKey)\n", rm.Key)
	for _, def := range derivedInitDefaults(rm.Fields) {
		fmt.Fprintf(&b, "\trec.Set(%q, %s)\n", def.name, def.literal)
	}
	b.WriteString("\treturn rec, nil\n}\n\n")

	if len(mergeEvents) > 0 {
		fmt.Fprintf(&b, "func (p *%s) applyMerge(ctx context.Context, ev events.Event) error {\n", typeName)
		b.WriteString("\tctx = writeguard.MarkInternal(ctx)\n")
		b.WriteString("\tvar data map[string]any\n")
		b.WriteString("\tif err := json.Unmarshal(ev.Data, &data); err != nil {\n\t\treturn err\n\t}\n")
		b.WriteString("\trec, err := p.getOrCreate(ev.AggregateID)\n\tif err != nil {\n\t\treturn err\n\t}\n")
		if len(plainColumns) > 0 {
			fmt.Fprintf(&b, "\tfor _, name := range []string{%s} {\n", quotedList(plainColumns))
			b.WriteString("\t\tif v, ok := data[name]; ok {\n\t\t\trec.Set(name, v)\n\t\t}\n\t}\n")
		}
		b.WriteString("\treturn p.app.SaveWithContext(ctx, rec)\n}\n\n")
	}

	if usesToggle {
		b.WriteString("// setToggle sets a toggle-derivation field, keyed by the triggering\n")
		b.WriteString("// event's own aggregate id (toggle events are always on the read model's\n")
		b.WriteString("// own stream, per eventmodelschema's fieldDerivation shape).\n")
		fmt.Fprintf(&b, "func (p *%s) setToggle(ctx context.Context, rowKey, field string, value bool) error {\n", typeName)
		b.WriteString("\tctx = writeguard.MarkInternal(ctx)\n")
		b.WriteString("\trec, err := p.getOrCreate(rowKey)\n\tif err != nil {\n\t\treturn err\n\t}\n")
		b.WriteString("\trec.Set(field, value)\n")
		b.WriteString("\treturn p.app.SaveWithContext(ctx, rec)\n}\n\n")
	}

	if usesCount {
		b.WriteString("// bumpCount adjusts a count-derivation field. rowKeyField names the\n")
		b.WriteString("// payload field on the triggering event carrying the TARGET row's key —\n")
		b.WriteString("// the event is usually on a different stream than this read model's own\n")
		b.WriteString("// (an assignment event rolling up onto its project), so the row can't be\n")
		b.WriteString("// found by ev.AggregateID the way a same-aggregate field can.\n")
		fmt.Fprintf(&b, "func (p *%s) bumpCount(ctx context.Context, ev events.Event, field, rowKeyField string, delta int) error {\n", typeName)
		b.WriteString("\tctx = writeguard.MarkInternal(ctx)\n")
		b.WriteString("\tvar data map[string]any\n\tif err := json.Unmarshal(ev.Data, &data); err != nil {\n\t\treturn err\n\t}\n")
		b.WriteString("\trowKey, _ := data[rowKeyField].(string)\n")
		b.WriteString("\trec, err := p.getOrCreate(rowKey)\n\tif err != nil {\n\t\treturn err\n\t}\n")
		b.WriteString("\trec.Set(field, rec.GetInt(field)+delta)\n")
		b.WriteString("\treturn p.app.SaveWithContext(ctx, rec)\n}\n\n")
	}

	if usesSum {
		b.WriteString("// bumpSum is bumpCount's sibling for a running total: amountField names\n")
		b.WriteString("// the numeric payload field to add or subtract, sign is +1/-1.\n")
		fmt.Fprintf(&b, "func (p *%s) bumpSum(ctx context.Context, ev events.Event, field, amountField, rowKeyField string, sign float64) error {\n", typeName)
		b.WriteString("\tctx = writeguard.MarkInternal(ctx)\n")
		b.WriteString("\tvar data map[string]any\n\tif err := json.Unmarshal(ev.Data, &data); err != nil {\n\t\treturn err\n\t}\n")
		b.WriteString("\trowKey, _ := data[rowKeyField].(string)\n")
		b.WriteString("\tamount, _ := data[amountField].(float64)\n")
		b.WriteString("\trec, err := p.getOrCreate(rowKey)\n\tif err != nil {\n\t\treturn err\n\t}\n")
		b.WriteString("\trec.Set(field, rec.GetFloat(field)+sign*amount)\n")
		b.WriteString("\treturn p.app.SaveWithContext(ctx, rec)\n}\n\n")
	}

	if usesGroupByCount || usesGroupBySum {
		b.WriteString("// groupByEntry finds-or-creates the ROW WITHIN field's own nested JSON\n")
		b.WriteString("// list matching groupKeyField/groupKeyValue, so bumpGroupByCount/\n")
		b.WriteString("// bumpGroupBySum share one read-modify-write instead of repeating it per\n")
		b.WriteString("// kind. The returned entry is already the last element of rows (or was\n")
		b.WriteString("// just appended as one) — since a map is a reference type, the caller's\n")
		b.WriteString("// mutations to entry are visible through rows without any index bookkeeping.\n")
		fmt.Fprintf(&b, "func (p *%s) groupByEntry(rec *core.Record, field, groupKeyField string, groupKeyValue any) (map[string]any, []map[string]any) {\n", typeName)
		b.WriteString("\tvar rows []map[string]any\n")
		b.WriteString("\t_ = rec.UnmarshalJSONField(field, &rows)\n")
		b.WriteString("\tfor _, entry := range rows {\n")
		b.WriteString("\t\tif entry[groupKeyField] == groupKeyValue {\n\t\t\treturn entry, rows\n\t\t}\n")
		b.WriteString("\t}\n")
		b.WriteString("\tentry := map[string]any{groupKeyField: groupKeyValue}\n")
		b.WriteString("\treturn entry, append(rows, entry)\n")
		b.WriteString("}\n\n")
	}

	if usesGroupByCount {
		b.WriteString("// bumpGroupByCount is bumpCount's nested-list sibling (schema 2.3.0's\n")
		b.WriteString("// groupBy derivation): rowKeyField names the payload field on the\n")
		b.WriteString("// triggering event carrying the TOP-LEVEL row's key (unrelated to\n")
		b.WriteString("// groupKeyField, which only picks the entry WITHIN that row's own list).\n")
		fmt.Fprintf(&b, "func (p *%s) bumpGroupByCount(ctx context.Context, ev events.Event, field, groupKeyField, subfield, rowKeyField string, delta int) error {\n", typeName)
		b.WriteString("\tctx = writeguard.MarkInternal(ctx)\n")
		b.WriteString("\tvar data map[string]any\n\tif err := json.Unmarshal(ev.Data, &data); err != nil {\n\t\treturn err\n\t}\n")
		b.WriteString("\trowKey, _ := data[rowKeyField].(string)\n")
		b.WriteString("\trec, err := p.getOrCreate(rowKey)\n\tif err != nil {\n\t\treturn err\n\t}\n")
		b.WriteString("\tentry, rows := p.groupByEntry(rec, field, groupKeyField, data[groupKeyField])\n")
		b.WriteString("\tcurrent, _ := entry[subfield].(float64)\n")
		b.WriteString("\tentry[subfield] = current + float64(delta)\n")
		b.WriteString("\trec.Set(field, rows)\n")
		b.WriteString("\treturn p.app.SaveWithContext(ctx, rec)\n}\n\n")
	}

	if usesGroupBySum {
		b.WriteString("// bumpGroupBySum is bumpGroupByCount's sibling for a running total within\n")
		b.WriteString("// a group entry: amountField names the numeric payload field to add or\n")
		b.WriteString("// subtract, sign is +1/-1.\n")
		fmt.Fprintf(&b, "func (p *%s) bumpGroupBySum(ctx context.Context, ev events.Event, field, groupKeyField, subfield, amountField, rowKeyField string, sign float64) error {\n", typeName)
		b.WriteString("\tctx = writeguard.MarkInternal(ctx)\n")
		b.WriteString("\tvar data map[string]any\n\tif err := json.Unmarshal(ev.Data, &data); err != nil {\n\t\treturn err\n\t}\n")
		b.WriteString("\trowKey, _ := data[rowKeyField].(string)\n")
		b.WriteString("\tamount, _ := data[amountField].(float64)\n")
		b.WriteString("\trec, err := p.getOrCreate(rowKey)\n\tif err != nil {\n\t\treturn err\n\t}\n")
		b.WriteString("\tentry, rows := p.groupByEntry(rec, field, groupKeyField, data[groupKeyField])\n")
		b.WriteString("\tcurrent, _ := entry[subfield].(float64)\n")
		b.WriteString("\tentry[subfield] = current + sign*amount\n")
		b.WriteString("\trec.Set(field, rows)\n")
		b.WriteString("\treturn p.app.SaveWithContext(ctx, rec)\n}\n\n")
	}

	writeProjectionScopes(&b, rm, typeName)
	return b.String()
}

// writeProjectionImports emits the import block every generated projection
// shares. json is only used by the generic merge path and by count/sum
// derivations — a read model with ONLY a toggle derivation and no plain
// merge events needs it emitted conditionally, or the generated file fails
// to compile with an unused import.
func writeProjectionImports(b *strings.Builder, usesJSON bool) {
	b.WriteString("import (\n")
	b.WriteString("\t\"context\"\n")
	b.WriteString("\t\"database/sql\"\n")
	if usesJSON {
		b.WriteString("\t\"encoding/json\"\n")
	}
	b.WriteString("\t\"errors\"\n\n")
	b.WriteString("\t\"github.com/pocketbase/pocketbase/core\"\n\n")
	b.WriteString("\t\"github.com/jamestryand/pocketcqrs/events\"\n")
	b.WriteString("\t\"github.com/jamestryand/pocketcqrs/writeguard\"\n")
	b.WriteString(")\n\n")
}

func writeProjectionHeader(b *strings.Builder, d Domain, rm ReadModel, ctor, typeName string) {
	fmt.Fprintf(b, "// %s projects %s events into the %q collection, one row per %s\n", ctor, d.Aggregate, rm.Collection, d.Aggregate)
	b.WriteString("// stream keyed by the aggregate id — a generic field-merge, the same shape\n")
	b.WriteString("// the JS scaffolder's project() produces. Port your own per-event rules\n")
	b.WriteString("// once they've settled; the collection itself becomes an ordinary\n")
	b.WriteString("// PocketBase migration (see migrations/1754200000_tasks_collection.go for\n")
	b.WriteString("// the shape) — this file does not create it.\n")
	fmt.Fprintf(b, "func %s(app core.App) *%s { return &%s{app: app} }\n\n", ctor, typeName, typeName)
	fmt.Fprintf(b, "type %s struct {\n\tapp core.App\n}\n\n", typeName)
	fmt.Fprintf(b, "func (p *%s) Name() string { return %q }\n", typeName, rm.Collection)
	fmt.Fprintf(b, "func (p *%s) Collections() []string { return []string{%q} }\n\n", typeName, rm.Collection)
}

// writeProjectionPlainApply is the original, pre-Finding-3 generic
// field-merge Apply — kept byte-for-byte unchanged for a read model with no
// derived fields, so an existing generated file only changes when the
// model actually asks for a fold.
func writeProjectionPlainApply(b *strings.Builder, rm ReadModel, on []string, typeName string) {
	columns := make([]string, 0, len(rm.Fields))
	for _, f := range rm.Fields {
		if f.Name != rm.Key {
			columns = append(columns, f.Name)
		}
	}
	sort.Strings(columns)

	fmt.Fprintf(b, "func (p *%s) Apply(ctx context.Context, ev events.Event) error {\n", typeName)
	b.WriteString("\tswitch ev.Type {\n")
	fmt.Fprintf(b, "\tcase %s:\n", quotedList(on))
	b.WriteString("\tdefault:\n\t\treturn nil\n\t}\n")
	b.WriteString("\tctx = writeguard.MarkInternal(ctx)\n\n")
	b.WriteString("\tvar data map[string]any\n")
	b.WriteString("\tif err := json.Unmarshal(ev.Data, &data); err != nil {\n\t\treturn err\n\t}\n\n")
	fmt.Fprintf(b, "\trec, err := p.app.FindFirstRecordByData(%q, %q, ev.AggregateID)\n", rm.Collection, rm.Key)
	b.WriteString("\tif err != nil && !errors.Is(err, sql.ErrNoRows) {\n\t\treturn err\n\t}\n")
	b.WriteString("\tif rec == nil {\n")
	fmt.Fprintf(b, "\t\tcol, err := p.app.FindCollectionByNameOrId(%q)\n", rm.Collection)
	b.WriteString("\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n")
	b.WriteString("\t\trec = core.NewRecord(col)\n")
	fmt.Fprintf(b, "\t\trec.Set(%q, ev.AggregateID)\n", rm.Key)
	b.WriteString("\t}\n")
	if len(columns) > 0 {
		fmt.Fprintf(b, "\tfor _, name := range []string{%s} {\n", quotedList(columns))
		b.WriteString("\t\tif v, ok := data[name]; ok {\n\t\t\trec.Set(name, v)\n\t\t}\n\t}\n")
	}
	b.WriteString("\treturn p.app.SaveWithContext(ctx, rec)\n")
	b.WriteString("}\n")
}

// writeProjectionScopes appends one Resolve<Param> helper per declared
// scope, independent of whether the read model has any derived fields.
// There is no generated query function anywhere in this package for a
// stateView read (reads go straight through PocketBase's own API in
// consumer code) — so a scoped query has nowhere existing to plug into.
// This generates the resolution half of the semi-join instead: the set of
// this read model's own key values the param allows, which the caller folds
// into whatever filter they build (see the recomputeTotals-style
// FindRecordsByFilter precedent in projections/orders.go).
func writeProjectionScopes(b *strings.Builder, rm ReadModel, typeName string) {
	for _, sc := range rm.Scopes {
		fmt.Fprintf(b, "// Resolve%s resolves the %q scope: %s on %q selects which\n",
			ExportName(sc.Param), sc.Param, sc.Via.MatchParamTo, sc.Via.Collection)
		fmt.Fprintf(b, "// %s values are in scope for a given %s. Build your own %q-IN-(...)\n",
			sc.Via.SelectField, sc.Param, sc.Via.FilterLocalField)
		b.WriteString("// PocketBase filter from the returned values.\n")
		fmt.Fprintf(b, "func (p *%s) Resolve%s(value string) ([]string, error) {\n", typeName, ExportName(sc.Param))
		fmt.Fprintf(b, "\trows, err := p.app.FindRecordsByFilter(%q, %q, \"\", -1, 0, map[string]any{\"v\": value})\n",
			sc.Via.Collection, sc.Via.MatchParamTo+" = {:v}")
		b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
		b.WriteString("\tids := make([]string, 0, len(rows))\n")
		b.WriteString("\tfor _, r := range rows {\n")
		fmt.Fprintf(b, "\t\tids = append(ids, r.GetString(%q))\n", sc.Via.SelectField)
		b.WriteString("\t}\n\treturn ids, nil\n}\n\n")
	}
}

// derivedAction is one event's effect on one derived field, resolved from a
// FieldDerivation's split on/off/increment/decrement/add/subtract id lists
// into a single per-event, per-field operation.
type derivedAction struct {
	field       string
	kind        string
	boolValue   bool
	delta       int
	sign        float64
	amountField string
	rowKeyField string // count/sum only; "" is unused by toggle

	// isGroupBy marks an action that bumps a SUBFIELD nested within field's
	// own grouped-rollup list (schema 2.3.0), rather than field itself as a
	// plain scalar column. kind/delta/sign/amountField above still describe
	// the SUBFIELD's own count/sum derivation; rowKeyField above is
	// unchanged too — it still resolves the TOP-LEVEL row, exactly as it
	// does for a plain count/sum column. groupKeyField/subfield below are
	// meaningful only when isGroupBy is true.
	isGroupBy bool
	// groupKeyField names the entry property, WITHIN field's own nested
	// list, that carries the group key (the parent Field.Derivation's own
	// GroupByField).
	groupKeyField string
	// subfield names the entry property this action bumps.
	subfield string
}

func (a derivedAction) call() string {
	if a.isGroupBy {
		switch a.kind {
		case DerivationCount:
			return fmt.Sprintf("p.bumpGroupByCount(ctx, ev, %q, %q, %q, %q, %d)",
				a.field, a.groupKeyField, a.subfield, a.rowKeyField, a.delta)
		case DerivationSum:
			return fmt.Sprintf("p.bumpGroupBySum(ctx, ev, %q, %q, %q, %q, %q, %g)",
				a.field, a.groupKeyField, a.subfield, a.amountField, a.rowKeyField, a.sign)
		default:
			return "nil" // unreachable: mapping/Validate reject a groupBy subfield of any other kind
		}
	}
	switch a.kind {
	case DerivationToggle:
		return fmt.Sprintf("p.setToggle(ctx, ev.AggregateID, %q, %t)", a.field, a.boolValue)
	case DerivationCount:
		return fmt.Sprintf("p.bumpCount(ctx, ev, %q, %q, %d)", a.field, a.rowKeyField, a.delta)
	case DerivationSum:
		return fmt.Sprintf("p.bumpSum(ctx, ev, %q, %q, %q, %g)", a.field, a.amountField, a.rowKeyField, a.sign)
	default:
		return "nil" // unreachable: Validate rejects any other kind before generation runs
	}
}

// collectDerivedActions indexes every derived field's trigger events into
// per-event action lists, plus the events in FIRST-SEEN order across fields
// (declaration order) so the generated switch is deterministic across
// regenerations of the same model — map iteration order is not.
func collectDerivedActions(fields []Field, rmKey string) (map[string][]derivedAction, []string) {
	actions := map[string][]derivedAction{}
	var order []string
	add := func(ev string, a derivedAction) {
		if _, ok := actions[ev]; !ok {
			order = append(order, ev)
		}
		actions[ev] = append(actions[ev], a)
	}
	for _, f := range fields {
		dv := f.Derivation
		if dv == nil {
			continue
		}
		switch dv.Kind {
		case DerivationToggle:
			for _, ev := range dv.OnEventIDs {
				add(ev, derivedAction{field: f.Name, kind: DerivationToggle, boolValue: true})
			}
			for _, ev := range dv.OffEventIDs {
				add(ev, derivedAction{field: f.Name, kind: DerivationToggle, boolValue: false})
			}
		case DerivationCount:
			rowKey := dv.RowKeyField
			if rowKey == "" {
				rowKey = rmKey
			}
			for _, ev := range dv.IncrementOnEventIDs {
				add(ev, derivedAction{field: f.Name, kind: DerivationCount, delta: 1, rowKeyField: rowKey})
			}
			for _, ev := range dv.DecrementOnEventIDs {
				add(ev, derivedAction{field: f.Name, kind: DerivationCount, delta: -1, rowKeyField: rowKey})
			}
		case DerivationSum:
			rowKey := dv.RowKeyField
			if rowKey == "" {
				rowKey = rmKey
			}
			for _, ev := range dv.AddOnEventIDs {
				add(ev, derivedAction{field: f.Name, kind: DerivationSum, sign: 1, amountField: dv.AmountField, rowKeyField: rowKey})
			}
			for _, ev := range dv.SubtractOnEventIDs {
				add(ev, derivedAction{field: f.Name, kind: DerivationSum, sign: -1, amountField: dv.AmountField, rowKeyField: rowKey})
			}
		case DerivationGroupBy:
			// f itself carries no trigger events of its own -- every action
			// comes from a SUBFIELD's own count/sum derivation, computed
			// within its group rather than across the whole read model. A
			// toggle subfield is rejected upstream (mapping.go / Validate),
			// so there is nothing to collect for one here; the subfield
			// named by dv.GroupByField carries no derivation at all (its
			// value is the group key itself), so it is skipped too.
			for _, sf := range f.Subfields {
				sdv := sf.Derivation
				if sdv == nil {
					continue
				}
				switch sdv.Kind {
				case DerivationCount:
					rowKey := sdv.RowKeyField
					if rowKey == "" {
						rowKey = rmKey
					}
					for _, ev := range sdv.IncrementOnEventIDs {
						add(ev, derivedAction{field: f.Name, kind: DerivationCount, delta: 1, rowKeyField: rowKey,
							isGroupBy: true, groupKeyField: dv.GroupByField, subfield: sf.Name})
					}
					for _, ev := range sdv.DecrementOnEventIDs {
						add(ev, derivedAction{field: f.Name, kind: DerivationCount, delta: -1, rowKeyField: rowKey,
							isGroupBy: true, groupKeyField: dv.GroupByField, subfield: sf.Name})
					}
				case DerivationSum:
					rowKey := sdv.RowKeyField
					if rowKey == "" {
						rowKey = rmKey
					}
					for _, ev := range sdv.AddOnEventIDs {
						add(ev, derivedAction{field: f.Name, kind: DerivationSum, sign: 1, amountField: sdv.AmountField, rowKeyField: rowKey,
							isGroupBy: true, groupKeyField: dv.GroupByField, subfield: sf.Name})
					}
					for _, ev := range sdv.SubtractOnEventIDs {
						add(ev, derivedAction{field: f.Name, kind: DerivationSum, sign: -1, amountField: sdv.AmountField, rowKeyField: rowKey,
							isGroupBy: true, groupKeyField: dv.GroupByField, subfield: sf.Name})
					}
				}
			}
		}
	}
	return actions, order
}

// plainColumnNames is columnNames restricted to fields with no derivation —
// a derived field is never generically copied from a same-named payload
// key, it is only ever set by its own dedicated helper.
func plainColumnNames(fields []Field, key string) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f.Name != key && f.Derivation == nil {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
}

// derivedFieldDefault is one derived field's zero-state initializer, applied
// when getOrCreate creates a brand new row so a field a fixture never
// exercises still reads as its declared initial value rather than a bare
// PocketBase zero-value gap.
type derivedFieldDefault struct{ name, literal string }

func derivedInitDefaults(fields []Field) []derivedFieldDefault {
	var out []derivedFieldDefault
	for _, f := range fields {
		dv := f.Derivation
		if dv == nil {
			continue
		}
		switch dv.Kind {
		case DerivationToggle:
			out = append(out, derivedFieldDefault{f.Name, fmt.Sprintf("%t", dv.Initial)})
		case DerivationCount:
			out = append(out, derivedFieldDefault{f.Name, "0"})
		case DerivationSum:
			out = append(out, derivedFieldDefault{f.Name, "0.0"})
		case DerivationGroupBy:
			// an empty list, not a nil field: a stateView read before any
			// contributing event has landed should see "no rows yet", not a
			// missing/null column.
			out = append(out, derivedFieldDefault{f.Name, "[]any{}"})
		}
	}
	return out
}

// subtractStrings returns from's entries that are not a key of the derived
// actions map, preserving from's own order.
func subtractStrings(from []string, derived map[string][]derivedAction) []string {
	out := make([]string, 0, len(from))
	for _, s := range from {
		if _, ok := derived[s]; !ok {
			out = append(out, s)
		}
	}
	return out
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
