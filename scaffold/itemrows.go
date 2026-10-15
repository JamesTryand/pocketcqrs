package scaffold

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// RowsPerListItem (schema 3.16.0): one row per group of a stream's list
// items, in place of one row per stream. ListField is a list field the read
// model's events carry (each carrying event replaces the list); GroupBy is
// the item subfield naming an item's group. Each row is keyed
// '<stream>|<group>'; its GroupBy field takes the group's value, its
// ListField the group's items, and its other plain fields are merged from
// the stream's events. An item without GroupBy is in no row.
type RowsPerListItem struct {
	ListField string `json:"listField"`
	GroupBy   string `json:"groupBy"`
}

// DerivationSumOfItems (schema 3.16.0, with rowsPerListItem only): the sum
// of an item subfield over the row's own items; null unless every item
// carries it.
const DerivationSumOfItems = "sumOfItems"

// ItemStreams is the companion collection a rowsPerListItem read model keeps
// its streams' state in (superusers only).
func ItemStreams(rm ReadModel) string { return rm.Collection + "_streams" }

// validateItemRows checks a rowsPerListItem read model, and that sumOfItems
// appears nowhere else.
func validateItemRows(rm ReadModel, collections map[string]bool, add func(string, ...any)) {
	if rm.RowsPerListItem == nil {
		for _, f := range rm.Fields {
			if f.Derivation != nil && f.Derivation.Kind == DerivationSumOfItems {
				add("read model %q: field %q is a sumOfItems derivation, which needs rowsPerListItem", rm.Collection, f.Name)
			}
		}
		return
	}
	rp := rm.RowsPerListItem
	if rm.RowKeyField != "" {
		add("read model %q: rowsPerListItem and rowKeyField can't both key its rows", rm.Collection)
	}
	if collections[ItemStreams(rm)] {
		add("read model %q: rowsPerListItem keeps its state in %q, which another read model already is", rm.Collection, ItemStreams(rm))
	}
	byName := map[string]Field{}
	for _, f := range rm.Fields {
		byName[f.Name] = f
	}
	if f, ok := byName[rp.ListField]; !ok || f.Type != "json" {
		add("read model %q: rowsPerListItem's listField %q must be a list field of the read model", rm.Collection, rp.ListField)
	}
	if _, ok := byName[rp.GroupBy]; !ok {
		add("read model %q: rowsPerListItem's groupBy %q must be a field of the read model, to hold the group", rm.Collection, rp.GroupBy)
	}
	for _, f := range rm.Fields {
		dv := f.Derivation
		if dv == nil {
			continue
		}
		switch dv.Kind {
		case DerivationByEvent:
		case DerivationSumOfItems:
			if dv.Subfield == "" {
				add("read model %q: field %q is a sumOfItems derivation but names no subfield", rm.Collection, f.Name)
			}
			if f.Type != "number" {
				add("read model %q: field %q is a sumOfItems derivation, so it must be a number", rm.Collection, f.Name)
			}
		default:
			add("read model %q: field %q is a %s derivation, which rowsPerListItem rows don't fold", rm.Collection, f.Name, dv.Kind)
		}
	}
}

// itemRowsParts is what both targets' projections are generated from.
type itemRowsParts struct {
	columns []string                  // plain fields merged from events
	sums    map[string]string         // field -> item subfield
	byEvent map[string]map[string]any // event -> field -> literal
}

func itemRowsPartsOf(rm ReadModel) itemRowsParts {
	p := itemRowsParts{sums: map[string]string{}, byEvent: byEventValues(rm)}
	for _, f := range rm.Fields {
		switch {
		case f.Name == rm.Key || f.Name == rm.RowsPerListItem.ListField || f.Name == rm.RowsPerListItem.GroupBy:
		case f.Derivation == nil:
			p.columns = append(p.columns, f.Name)
		case f.Derivation.Kind == DerivationSumOfItems:
			p.sums[f.Name] = f.Derivation.Subfield
		}
	}
	sort.Strings(p.columns)
	return p
}

// itemRowsProjection is the JS projection of a rowsPerListItem read model:
// one itemRows op per event, which the host plans (functions/itemrows.go)
// against the stream's state in its companion collection.
func (d Domain) itemRowsProjection(rm ReadModel) string {
	on := rm.On
	if len(on) == 0 {
		on = d.Events()
	}
	rp := rm.RowsPerListItem
	parts := itemRowsPartsOf(rm)

	columns := []string{rm.Key + ":text"}
	for _, f := range rm.Fields {
		if f.Name != rm.Key {
			columns = append(columns, f.Name+":"+f.Type)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "//@trigger projection %s on %s\n", rm.Collection, strings.Join(on, " "))
	fmt.Fprintf(&b, "//@schema %s %s\n", rm.Collection, strings.Join(columns, " "))
	fmt.Fprintf(&b, "//@key %s\n", rm.Key)
	if rule, ok := d.readRule(rm); ok {
		b.WriteString("// Who may read it (requiredRole 2.7.0, selfAccess 3.2.0, grantsAccess 3.8.0),\n")
		b.WriteString("// enforced by PocketBase on every list, view and realtime subscription.\n")
		fmt.Fprintf(&b, "//@rule %s %s\n", rm.Collection, rule)
	}
	b.WriteString("// Each stream's own state (its merged fields, its list, the groups it has\n")
	b.WriteString("// rows for), kept by the host so this file never reads its rows back.\n")
	fmt.Fprintf(&b, "//@schema %s stream:text state:json\n", ItemStreams(rm))
	b.WriteString("//@key stream\n")
	fmt.Fprintf(&b, "//@rule %s superuser\n", ItemStreams(rm))
	b.WriteString("//\n")
	fmt.Fprintf(&b, "// rowsPerListItem (schema 3.16.0): one row per %s of a %s stream's %s,\n", rp.GroupBy, d.Aggregate, rp.ListField)
	fmt.Fprintf(&b, "// keyed '<stream>|<%s>'. An event carrying %s replaces the list; the\n", rp.GroupBy, rp.ListField)
	b.WriteString("// other fields merge from the stream's events onto every row. An item with\n")
	fmt.Fprintf(&b, "// no %s is in no row.", rp.GroupBy)
	if len(parts.sums) > 0 {
		b.WriteString(" A sumOfItems field is null until every item of its\n// row carries the subfield.")
	}
	b.WriteString("\n\n")

	cols, _ := json.Marshal(append([]string{}, parts.columns...))
	sums, _ := json.Marshal(parts.sums)
	values, _ := json.Marshal(parts.byEvent)
	removals, _ := json.Marshal(append([]string{}, rm.RemovedBy...))
	fmt.Fprintf(&b, "const columns = %s;\n", cols)
	fmt.Fprintf(&b, "const byEventValues = %s;\n\n", values)
	b.WriteString("function project(event) {\n")
	fmt.Fprintf(&b, "  var op = { stream: event.aggregateId, streams: %q, listField: %q, groupBy: %q,\n", ItemStreams(rm), rp.ListField, rp.GroupBy)
	fmt.Fprintf(&b, "    sums: %s, fields: {} };\n", sums)
	fmt.Fprintf(&b, "  if (%s.includes(event.type)) {\n", removals)
	fmt.Fprintf(&b, "    op.remove = true;\n    return { collection: %q, itemRows: op };\n  }\n", rm.Collection)
	b.WriteString("  var data = event.data || {};\n")
	b.WriteString("  for (const name of Object.keys(data)) {\n")
	b.WriteString("    if (columns.includes(name)) { op.fields[name] = data[name]; }\n  }\n")
	b.WriteString("  Object.assign(op.fields, byEventValues[event.type] || {});\n")
	fmt.Fprintf(&b, "  if (%q in data) { op.items = data[%q]; }\n", rp.ListField, rp.ListField)
	fmt.Fprintf(&b, "  return { collection: %q, itemRows: op };\n}\n", rm.Collection)
	writeJSProjectionScopes(&b, rm)
	return b.String()
}

// itemRowsProjectionGo is the Go projection of a rowsPerListItem read model:
// the same plan (package itemrows) the JS host makes, applied here.
func (d Domain) itemRowsProjectionGo(pkg string, rm ReadModel) string {
	on := rm.On
	if len(on) == 0 {
		on = d.Events()
	}
	rp := rm.RowsPerListItem
	parts := itemRowsPartsOf(rm)
	typeName := ExportName(rm.Collection) + "Projection"
	ctor := "New" + ExportName(rm.Collection)
	streams := ItemStreams(rm)

	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	b.WriteString("import (\n\t\"context\"\n\t\"database/sql\"\n\t\"encoding/json\"\n\t\"errors\"\n\n")
	b.WriteString("\t\"github.com/pocketbase/pocketbase/core\"\n\n")
	b.WriteString("\t\"github.com/jamestryand/pocketcqrs/events\"\n")
	b.WriteString("\t\"github.com/jamestryand/pocketcqrs/projections/itemrows\"\n")
	b.WriteString("\t\"github.com/jamestryand/pocketcqrs/writeguard\"\n)\n\n")

	fmt.Fprintf(&b, "// %s projects %s events into the %q collection: rowsPerListItem\n", ctor, d.Aggregate, rm.Collection)
	fmt.Fprintf(&b, "// (schema 3.16.0), one row per %s of a stream's %s, keyed\n", rp.GroupBy, rp.ListField)
	fmt.Fprintf(&b, "// '<stream>|<%s>'. Each stream's state is kept in %q (stream:text,\n", rp.GroupBy, streams)
	b.WriteString("// state:json; superusers only). Both collections are ordinary PocketBase\n")
	b.WriteString("// migrations (see migrations/1754200000_tasks_collection.go for the\n")
	b.WriteString("// shape) — this file does not create them.\n")
	fmt.Fprintf(&b, "func %s(app core.App) *%s { return &%s{app: app} }\n\n", ctor, typeName, typeName)
	fmt.Fprintf(&b, "type %s struct {\n\tapp core.App\n}\n\n", typeName)
	fmt.Fprintf(&b, "func (p *%s) Name() string { return %q }\n", typeName, rm.Collection)
	fmt.Fprintf(&b, "func (p *%s) Collections() []string { return []string{%q, %q} }\n\n", typeName, rm.Collection, streams)

	fmt.Fprintf(&b, "func (p *%s) Apply(ctx context.Context, ev events.Event) error {\n", typeName)
	fmt.Fprintf(&b, "\tswitch ev.Type {\n\tcase %s:\n\tdefault:\n\t\treturn nil\n\t}\n", quotedList(dedupe(append(append([]string{}, on...), rm.RemovedBy...))))
	b.WriteString("\tctx = writeguard.MarkInternal(ctx)\n")
	sums := make([]string, 0, len(parts.sums))
	for f, sub := range parts.sums {
		sums = append(sums, fmt.Sprintf("%q: %q", f, sub))
	}
	sort.Strings(sums)
	fmt.Fprintf(&b, "\top := itemrows.Op{Stream: ev.AggregateID, ListField: %q, GroupBy: %q,\n", rp.ListField, rp.GroupBy)
	fmt.Fprintf(&b, "\t\tSums: map[string]string{%s}, Fields: map[string]any{}}\n", strings.Join(sums, ", "))
	if len(rm.RemovedBy) > 0 {
		fmt.Fprintf(&b, "\tif ev.Type == %s {\n\t\top.Remove = true\n\t} else {\n", strings.Join(quoteEach(rm.RemovedBy), " || ev.Type == "))
	} else {
		b.WriteString("\t{\n")
	}
	b.WriteString("\t\tvar data map[string]any\n")
	b.WriteString("\t\tif err := json.Unmarshal(ev.Data, &data); err != nil {\n\t\t\treturn err\n\t\t}\n")
	if len(parts.columns) > 0 {
		fmt.Fprintf(&b, "\t\tfor _, name := range []string{%s} {\n", strings.Join(quoteEach(parts.columns), ", "))
		b.WriteString("\t\t\tif v, ok := data[name]; ok {\n\t\t\t\top.Fields[name] = v\n\t\t\t}\n\t\t}\n")
	}
	if len(parts.byEvent) > 0 {
		evs := make([]string, 0, len(parts.byEvent))
		for ev := range parts.byEvent {
			evs = append(evs, ev)
		}
		sort.Strings(evs)
		b.WriteString("\t\tswitch ev.Type {\n")
		for _, ev := range evs {
			fields := make([]string, 0, len(parts.byEvent[ev]))
			for f := range parts.byEvent[ev] {
				fields = append(fields, f)
			}
			sort.Strings(fields)
			fmt.Fprintf(&b, "\t\tcase %q:\n", ev)
			for _, f := range fields {
				fmt.Fprintf(&b, "\t\t\top.Fields[%q] = %s\n", f, goLiteral(parts.byEvent[ev][f]))
			}
		}
		b.WriteString("\t\t}\n")
	}
	fmt.Fprintf(&b, "\t\tif items, ok := data[%q]; ok {\n", rp.ListField)
	b.WriteString("\t\t\top.HasItems = true\n\t\t\top.Items, _ = items.([]any)\n\t\t}\n\t}\n\n")

	fmt.Fprintf(&b, "\tstate, err := p.app.FindFirstRecordByData(%q, \"stream\", ev.AggregateID)\n", streams)
	b.WriteString("\tif err != nil && !errors.Is(err, sql.ErrNoRows) {\n\t\treturn err\n\t}\n")
	b.WriteString("\tvar prior itemrows.State\n")
	b.WriteString("\tif state != nil {\n\t\tif err := state.UnmarshalJSONField(\"state\", &prior); err != nil {\n\t\t\treturn err\n\t\t}\n\t}\n")
	b.WriteString("\tnext, drop, rows := op.Plan(prior)\n")
	b.WriteString("\tfor _, key := range drop {\n")
	fmt.Fprintf(&b, "\t\trec, err := p.app.FindFirstRecordByData(%q, %q, key)\n", rm.Collection, rm.Key)
	b.WriteString("\t\tif errors.Is(err, sql.ErrNoRows) {\n\t\t\tcontinue\n\t\t}\n")
	b.WriteString("\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n")
	b.WriteString("\t\tif err := p.app.DeleteWithContext(ctx, rec); err != nil {\n\t\t\treturn err\n\t\t}\n\t}\n")
	b.WriteString("\tfor _, row := range rows {\n")
	fmt.Fprintf(&b, "\t\trec, err := p.record(%q, %q, row.Key)\n", rm.Collection, rm.Key)
	b.WriteString("\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n")
	fmt.Fprintf(&b, "\t\tfor field, value := range row.Fields {\n\t\t\tif field != %q {\n\t\t\t\trec.Set(field, value)\n\t\t\t}\n\t\t}\n", rm.Key)
	b.WriteString("\t\tif err := p.app.SaveWithContext(ctx, rec); err != nil {\n\t\t\treturn err\n\t\t}\n\t}\n")
	b.WriteString("\tif next == nil {\n\t\tif state == nil {\n\t\t\treturn nil\n\t\t}\n\t\treturn p.app.DeleteWithContext(ctx, state)\n\t}\n")
	b.WriteString("\tif state == nil {\n")
	fmt.Fprintf(&b, "\t\tif state, err = p.record(%q, \"stream\", ev.AggregateID); err != nil {\n\t\t\treturn err\n\t\t}\n\t}\n", streams)
	b.WriteString("\tstate.Set(\"state\", next)\n")
	b.WriteString("\treturn p.app.SaveWithContext(ctx, state)\n}\n\n")

	b.WriteString("// record is the row of collection whose keyField is key, new if there is none.\n")
	fmt.Fprintf(&b, "func (p *%s) record(collection, keyField, key string) (*core.Record, error) {\n", typeName)
	b.WriteString("\trec, err := p.app.FindFirstRecordByData(collection, keyField, key)\n")
	b.WriteString("\tif err == nil {\n\t\treturn rec, nil\n\t}\n")
	b.WriteString("\tif !errors.Is(err, sql.ErrNoRows) {\n\t\treturn nil, err\n\t}\n")
	b.WriteString("\tcol, err := p.app.FindCollectionByNameOrId(collection)\n")
	b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
	b.WriteString("\trec = core.NewRecord(col)\n\trec.Set(keyField, key)\n\treturn rec, nil\n}\n")
	writeProjectionScopes(&b, rm, typeName)
	return b.String()
}

func quoteEach(list []string) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}
