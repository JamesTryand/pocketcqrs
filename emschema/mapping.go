package emschema

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jamestryand/pocketcqrs/scaffold"
)

// Options carry the decisions an operator makes on a document's behalf.
type Options struct {
	// AggregateOverrides maps a schema element id to an aggregate name, and
	// is consulted ONLY where the document itself gives no tag.
	//
	// The `aggregate` field is optional in the schema and the worked example
	// deliberately leaves boundary elements untagged ("a boundary
	// notification isn't targeting a domain aggregate at all"). But this
	// project's write side is aggregate-organised throughout, and an
	// automation's resultEventIds ARE real log events, so something must own
	// that stream. Rather than guess — deriving from swimlaneId would
	// silently merge unrelated stream families into one, invisibly, until
	// the log is already wrong — the operator states the intent here.
	AggregateOverrides map[string]string
}

// Mapped is a document translated into this project's terms.
type Mapped struct {
	// Domains is one entry per aggregate, in stable name order.
	Domains []scaffold.Domain
	// DomainDocs is markdown per aggregate, carrying the methodology prose
	// that has no home in code.
	DomainDocs map[string]string
	Report     *Report
}

// Map translates a document into scaffold Domains.
//
// The importer's job is `document -> scaffold.Domain`, NOT
// `document -> JavaScript`: Generate() stays the only place that emits
// source, so the wizard and this importer cannot grow separate opinions
// about what a decider looks like.
func Map(doc *Document, opts Options) (*Mapped, error) {
	rep := Lint(doc)
	if err := rep.Err(); err != nil {
		return &Mapped{Report: rep}, err
	}

	m := &mapper{doc: doc, opts: opts, rep: rep, byAggregate: map[string]*scaffold.Domain{}}
	m.mapSlices()
	m.mapReadModels()
	if err := rep.Err(); err != nil {
		return &Mapped{Report: rep}, err
	}

	out := &Mapped{Report: rep, DomainDocs: map[string]string{}}
	names := make([]string, 0, len(m.byAggregate))
	for name := range m.byAggregate {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		d := *m.byAggregate[name]
		if err := d.Validate(); err != nil {
			rep.errorf("aggregate %q: %v", name, err)
			continue
		}
		// the generator's own view of what is unfinished joins the report,
		// so an operator sees one list rather than two
		rep.Warnings = append(rep.Warnings, d.Warnings()...)
		out.Domains = append(out.Domains, d)
		out.DomainDocs[name] = m.domainDoc(name, d)
	}
	return out, rep.Err()
}

type mapper struct {
	doc         *Document
	opts        Options
	rep         *Report
	byAggregate map[string]*scaffold.Domain
	// eventAggregate remembers where each event id landed, so a read model
	// and a reactor can find the aggregate that owns their events.
	eventAggregate map[string]string
}

func (m *mapper) domain(name string) *scaffold.Domain {
	d, ok := m.byAggregate[name]
	if !ok {
		d = &scaffold.Domain{Aggregate: name}
		m.byAggregate[name] = d
	}
	return d
}

// aggregateFor resolves the aggregate that owns an element, in the decided
// precedence: the document's own tag, then the operator's override, then a
// refusal naming the element.
func (m *mapper) aggregateFor(kind, id, tagged string) (string, bool) {
	// lower-camel is this project's aggregate convention ("order",
	// "supportTicket") while the schema's tag is PascalCase ("Order"), so
	// the case is normalised here rather than leaking a foreign convention
	// into every generated file name and gateway route
	if tagged != "" {
		return LowerFirst(scaffold.SanitizeName(tagged)), true
	}
	if override, ok := m.opts.AggregateOverrides[id]; ok && override != "" {
		m.rep.warnf("%s %q has no `aggregate` tag; using the supplied override %q", kind, id, override)
		return LowerFirst(scaffold.SanitizeName(override)), true
	}
	m.rep.errorf("%s %q has no `aggregate` tag and no override was supplied. "+
		"This project's write side is organised by aggregate and an automation's result events are "+
		"real log entries, so something must own the stream. Supply --aggregate %s=<name>, "+
		"or add the tag to the document; deriving one from the swimlane would silently merge "+
		"unrelated stream families", kind, id, id)
	return "", false
}

func (m *mapper) mapSlices() {
	m.eventAggregate = map[string]string{}
	for _, s := range m.doc.Slices {
		switch s.Pattern {
		case PatternStateChange:
			m.mapStateChange(s)
		case PatternAutomation:
			m.mapAutomation(s)
		case PatternStateView:
			// the read model itself is mapped separately; the slice adds
			// only a screen, which has no runtime concept here
		}
	}
}

func (m *mapper) mapStateChange(s Slice) {
	cmd := m.doc.Commands[s.CommandID]
	agg, ok := m.aggregateFor("command", s.CommandID, cmd.Aggregate)
	if !ok {
		return
	}
	d := m.domain(agg)
	d.Commands = append(d.Commands, m.command(agg, s.CommandID, cmd, s.EventIDs, isCreate(s)))
}

// mapAutomation turns an automation slice into a reactor plus the command it
// dispatches. `translation`, `Bridge` and `Reactor` are three vocabularies
// for this one shape — event(s) -> command -> event(s).
func (m *mapper) mapAutomation(s Slice) {
	cmd := m.doc.Commands[s.CommandID]
	agg, ok := m.aggregateFor("command", s.CommandID, cmd.Aggregate)
	if !ok {
		return
	}
	// the reactor belongs to whichever aggregate OWNS the trigger events —
	// that is the stream it consumes, and it may well not be the target
	source := agg
	for _, id := range s.TriggerEventIDs {
		if owner, ok := m.eventAggregate[id]; ok {
			source = owner
			break
		}
	}

	// An automation that dispatches ACROSS aggregates opens a new stream
	// every time it fires — the reactor derives the target id from the
	// source event, so there is one target instance per trigger. That makes
	// the dispatched command a create. An automation whose target is its own
	// trigger's aggregate (auto-ship an order) is the opposite: the stream
	// already exists. Getting this wrong makes the command's own scenario
	// fail with "does not exist", which is how it was found.
	crossAggregate := source != agg
	target := m.domain(agg)
	target.Commands = append(target.Commands,
		m.command(agg, s.CommandID, cmd, s.ResultEventIDs, crossAggregate))
	triggers := make([]string, 0, len(s.TriggerEventIDs))
	for _, id := range s.TriggerEventIDs {
		triggers = append(triggers, m.eventType(id))
	}
	if s.ReadModelID != "" {
		m.rep.warnf("automation %q consults read model %q; the generated reactor does not read it — "+
			"`pb.findRecord`/`pb.query` are available inside reactTo() if the rule needs it",
			s.ID, s.ReadModelID)
	}
	m.domain(source).Reactors = append(m.domain(source).Reactors, scaffold.Reactor{
		Name:      LowerFirst(scaffold.SanitizeName(TypeName(s.Name, s.ID))),
		On:        triggers,
		Aggregate: agg,
		Command:   m.commandName(s.CommandID),
		IDPrefix:  DeriveID(TypeName(s.Name, s.ID)) + "-",
	})
}

// command builds one scaffold command and its events, folding field types
// and reporting every fold that loses something.
func (m *mapper) command(agg, id string, c Command, eventIDs []string, once bool) scaffold.Command {
	out := scaffold.Command{
		Name:             m.commandName(id),
		Fields:           m.fields("command "+id, c.Fields),
		Once:             once,
		RequiresExisting: !once,
	}
	for _, evID := range eventIDs {
		ev := m.doc.Events[evID]
		m.eventAggregate[evID] = agg
		e := scaffold.Event{Name: m.eventType(evID)}
		if len(ev.Fields) > 0 {
			e.Fields = m.fields("event "+evID, ev.Fields)
		} else {
			// The schema makes `fields` optional throughout, so this is the
			// common case for a v1-era document. Saying "no payload" out
			// loud is what stops it reading as "nobody has specified this",
			// and the count says how much of the document was silent.
			e.NoFields = true
			m.rep.warnf("event %q declares no fields; generated as carrying no payload", evID)
		}
		out.Events = append(out.Events, e)
	}
	return out
}

func (m *mapper) fields(owner string, in []Field) []scaffold.Field {
	out := make([]scaffold.Field, 0, len(in))
	for _, f := range in {
		if note := FoldNote(owner, f); note != "" {
			m.rep.warnf("%s", note)
		}
		out = append(out, scaffold.Field{
			Name: scaffold.SanitizeName(f.Name),
			Type: FoldType(f),
		})
	}
	return out
}

// mapReadModels attaches each read model to the aggregate owning the events
// it folds. A read model spanning several is legitimate — the schema gives
// them no aggregate precisely because they are cross-cutting — so it lands
// on the first owner and the span is reported.
func (m *mapper) mapReadModels() {
	ids := make([]string, 0, len(m.doc.ReadModels))
	for id := range m.doc.ReadModels {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		rm := m.doc.ReadModels[id]
		owners := map[string]bool{}
		on := make([]string, 0, len(rm.BuiltFromEventIDs))
		for _, evID := range rm.BuiltFromEventIDs {
			on = append(on, m.eventType(evID))
			if owner, ok := m.eventAggregate[evID]; ok {
				owners[owner] = true
			}
		}
		if len(on) == 0 {
			m.rep.warnf("read model %q lists no builtFromEventIds, so the generated projection would "+
				"never fire; skipped", id)
			continue
		}
		owner := ""
		for name := range owners {
			if owner == "" || name < owner {
				owner = name
			}
		}
		if owner == "" {
			m.rep.errorf("read model %q folds events owned by no aggregate this document defines", id)
			continue
		}
		if len(owners) > 1 {
			m.rep.warnf("read model %q folds events from %d aggregates; its projection is generated "+
				"under %q but listens to all of them, which is what a cross-cutting read model means",
				id, len(owners), owner)
		}

		key, keyNote := m.readModelKey(id, rm, owner)
		if keyNote != "" {
			m.rep.warnf("%s", keyNote)
		}
		m.domain(owner).ReadModels = append(m.domain(owner).ReadModels, scaffold.ReadModel{
			Collection: scaffold.SanitizeName(collectionName(rm.Name, id)),
			Key:        key,
			Fields:     m.fields("read model "+id, rm.Fields),
			On:         on,
		})
	}
}

// readModelKey picks the row key. idAttribute is a gift here: it names the
// field carrying identity, which is exactly what the projection keys on.
func (m *mapper) readModelKey(id string, rm ReadModel, owner string) (string, string) {
	for _, f := range rm.Fields {
		if f.IDAttribute {
			return scaffold.SanitizeName(f.Name), ""
		}
	}
	fallback := owner + "Id"
	return fallback, fmt.Sprintf(
		"read model %q marks no field with idAttribute; keying rows on %q (the aggregate id)", id, fallback)
}

func (m *mapper) eventType(id string) string {
	ev, ok := m.doc.Events[id]
	if !ok {
		return TypeName("", id)
	}
	return TypeName(ev.Name, id)
}

func (m *mapper) commandName(id string) string {
	c, ok := m.doc.Commands[id]
	if !ok {
		return TypeName("", id)
	}
	return TypeName(c.Name, id)
}

// collectionName renders a read-model name as a collection identifier.
func collectionName(name, id string) string {
	return LowerFirst(TypeName(name, id))
}

// isCreate decides which command is the aggregate's "create".
//
// A scenario with an EMPTY `given` is a command applied to a stream that
// does not exist yet — the document's own statement that this is the
// beginning. That is a real signal rather than a guess, and where no
// scenario says so the first command of the aggregate is used.
func isCreate(s Slice) bool {
	for _, sc := range s.Scenarios {
		if sc.Kind == KindStateChange && len(sc.Given) == 0 {
			return true
		}
	}
	return false
}

// domainDoc renders the methodology prose that has no home in code.
//
// The importer writes this itself rather than leaving it to the catalog:
// Catalog.Skeleton builds from the RUNNING system's declared metadata, so
// prose living in comments is invisible to it. The importer holds the whole
// document, so it can carry reason, question, descriptions, business
// capability, chapters and hotspots — and WriteSkeletons skips a file that
// already exists, so this survives later `catalog --skeletons` runs.
func (m *mapper) domainDoc(agg string, d scaffold.Domain) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", agg)
	fmt.Fprintf(&b, "Imported from the EventModeling document %q (`%s`).\n\n", m.doc.Name, m.doc.ID)
	if m.doc.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", m.doc.Description)
	}

	b.WriteString("## Commands\n\n")
	b.WriteString("| command | reason | description |\n| --- | --- | --- |\n")
	for _, c := range d.Commands {
		reason, desc := "", ""
		for id, sc := range m.doc.Commands {
			if m.commandName(id) == c.Name {
				reason, desc = sc.Reason, sc.Description
				break
			}
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", c.Name, orDash(reason), orDash(desc))
	}

	if len(d.ReadModels) > 0 {
		b.WriteString("\n## Read models\n\n")
		b.WriteString("| collection | question | description |\n| --- | --- | --- |\n")
		for _, rm := range d.ReadModels {
			question, desc := "", ""
			for id, srm := range m.doc.ReadModels {
				if scaffold.SanitizeName(collectionName(srm.Name, id)) == rm.Collection {
					question, desc = srm.Question, srm.Description
					break
				}
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s |\n", rm.Collection, orDash(question), orDash(desc))
		}
	}

	if len(m.doc.Hotspots) > 0 {
		b.WriteString("\n## Open questions\n\n")
		ids := make([]string, 0, len(m.doc.Hotspots))
		for id := range m.doc.Hotspots {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			h := m.doc.Hotspots[id]
			state := "open"
			if h.Resolved {
				state = "resolved"
			}
			fmt.Fprintf(&b, "- **%s** (%s) — %s\n", id, state, h.Note)
		}
	}

	b.WriteString("\n---\n\nGenerated by `pocketcqrs schema import`. " +
		"Edit freely: a re-import will not overwrite a file that already exists.\n")
	return b.String()
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
