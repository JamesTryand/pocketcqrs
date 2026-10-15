package emschema

import (
	"encoding/json"
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

	// RoleField and SubjectField name the auth-record fields the generated
	// read rules read a caller's role and data-subject id from ("" = "role"
	// and the record id); match the server's --cqrsRoleField and
	// --cqrsSubjectField.
	RoleField, SubjectField string
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
		d.RoleField, d.SubjectField = opts.RoleField, opts.SubjectField
		// schema 3.8.0: a read model declaring no access rule is read by
		// every signed-in caller (decision 8, 2026-10-15)
		d.SignedInByDefault = true
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
	// and a reactor can find the aggregate that owns their events. Built
	// incrementally as commands are mapped.
	eventAggregate map[string]string
	// eventOwners resolves which aggregate's stream every produced event
	// belongs to, statically from each slice's own command tag and
	// independent of mapping order -- hasCreateEvidence / hasUpdateEvidence
	// need it for every given event up front, including ones from slices
	// later in document order.
	eventOwners map[string]string
	// eventEndsStream marks the ids of events tagged `endsStream: true` in
	// the source document -- scenarioNetExists needs this to fold a
	// scenario's `given` into a net "does the stream currently exist"
	// verdict rather than just scanning for own-stream presence. Finding 3.
	eventEndsStream map[string]bool
	prefixMin       map[string]int // hashed prefix filters: minPrefixLength per (read model, field, normalizer)
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
	m.eventOwners = m.buildEventOwners()
	m.eventEndsStream = m.buildEventEndsStream()
	// stateChange slices first, then automations: mapAutomation resolves a
	// reactor's source aggregate from the incrementally-built eventAggregate
	// map, and its trigger events are produced by stateChange slices, so
	// those must map before the automation pass consults it.
	for _, s := range m.doc.Slices {
		if s.Pattern == PatternStateChange {
			m.mapStateChange(s)
		}
	}
	for _, s := range m.doc.Slices {
		if s.Pattern == PatternAutomation {
			m.mapAutomation(s)
		}
	}
	// PatternStateView: the read model itself is mapped separately; the slice
	// adds only a screen, which has no runtime concept here.
}

// buildEventOwners resolves, for every event a stateChange or automation
// slice produces, which aggregate's stream it belongs to -- statically, from
// each slice's own command tag, independent of document order.
// hasCreateEvidence needs this to tell a scenario's own-aggregate `given`
// (real evidence the stream already exists) apart from a cross-aggregate
// precondition (a different stream's event, which says nothing about this one).
func (m *mapper) buildEventOwners() map[string]string {
	owners := map[string]string{}
	assign := func(agg string, eventIDs []string) {
		for _, id := range eventIDs {
			owners[id] = agg
		}
	}
	// resolveAggregate is aggregateFor without the report side effects: this
	// pre-pass only needs the name, and the real mapping pass reports every
	// missing tag / applied override once, on its own call.
	resolve := func(id, tagged string) string {
		if tagged != "" {
			return LowerFirst(scaffold.SanitizeName(tagged))
		}
		if override, ok := m.opts.AggregateOverrides[id]; ok && override != "" {
			return LowerFirst(scaffold.SanitizeName(override))
		}
		return ""
	}
	for _, s := range m.doc.Slices {
		if s.Pattern == PatternStateChange {
			if agg := resolve(s.CommandID, m.doc.Commands[s.CommandID].Aggregate); agg != "" {
				assign(agg, s.EventIDs)
			}
		}
	}
	for _, s := range m.doc.Slices {
		if s.Pattern == PatternAutomation {
			if agg := resolve(s.CommandID, m.doc.Commands[s.CommandID].Aggregate); agg != "" {
				assign(agg, s.ResultEventIDs)
			}
		}
	}
	return owners
}

// buildEventEndsStream indexes every event the document tags
// `endsStream: true`, by id. Cheap and total over m.doc.Events rather than
// scoped to owned events the way buildEventOwners is -- scenarioNetExists
// only ever looks an id up after already confirming it belongs to the
// scenario's own aggregate, so an unscoped set costs nothing extra and
// needs no ordering relative to the owners pass.
func (m *mapper) buildEventEndsStream() map[string]bool {
	ends := map[string]bool{}
	for id, ev := range m.doc.Events {
		if ev.EndsStream {
			ends[id] = true
		}
	}
	return ends
}

func (m *mapper) mapStateChange(s Slice) {
	cmd := m.doc.Commands[s.CommandID]
	agg, ok := m.aggregateFor("command", s.CommandID, cmd.Aggregate)
	if !ok {
		return
	}
	// A command with scenario evidence for BOTH a fresh stream
	// (hasCreateEvidence) and an already-existing one (hasUpdateEvidence) is a
	// genuine upsert: neither flag is set, so the decider template emits no
	// existence check at all and the command always succeeds. With only
	// create evidence it is the ordinary create-only rule; with neither, the
	// pre-existing default (RequiresExisting) is unchanged.
	hasCreate := m.hasCreateEvidence(s, agg)
	hasUpdate := m.hasUpdateEvidence(s, agg)
	once := hasCreate && !hasUpdate
	requiresExisting := !hasCreate
	d := m.domain(agg)
	c := m.command(agg, s.CommandID, cmd, s.EventIDs, once, requiresExisting)
	c.CanBeginAgain = once && m.hasBeginAgainEvidence(s, agg)
	d.Commands = append(d.Commands, c)
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

	// An automation that dispatches ACROSS aggregates derives the target id
	// from the source event, so it opens a new target stream per fire — but
	// only when this slice's own scenario evidence says so, the same rule
	// hasCreateEvidence applies to a directly-invoked command: a `given`
	// event on the TARGET aggregate's own stream is real evidence the stream
	// already exists (the fan-out-lock case: log an entry, then an invoice
	// reaction locks it), while a foreign-aggregate given (the trigger's own
	// history) is not. This also lets two independent automations/commands
	// each genuinely create the same aggregate TYPE (two triggers each
	// raising a fresh notification) without one disqualifying the other,
	// which the old "has this aggregate got a create yet" check could not. An
	// automation whose target is its own trigger's aggregate (auto-ship an
	// order) is never a create either. Getting this wrong makes the command's
	// own scenario fail with "does not exist", which is how it was found.
	crossAggregate := source != agg
	target := m.domain(agg)
	isCreate := crossAggregate && m.hasCreateEvidence(s, agg)
	c := m.command(agg, s.CommandID, cmd, s.ResultEventIDs, isCreate, !isCreate)
	c.CanBeginAgain = isCreate && m.hasBeginAgainEvidence(s, agg)
	target.Commands = append(target.Commands, c)
	triggers := make([]string, 0, len(s.TriggerEventIDs))
	for _, id := range s.TriggerEventIDs {
		triggers = append(triggers, m.eventType(id))
	}
	if s.ReadModelID != "" {
		m.rep.warnf("automation %q consults read model %q; the generated reactor does not read it — "+
			"`pb.findRecord`/`pb.query` are available inside reactTo() if the rule needs it",
			s.ID, s.ReadModelID)
	}
	// Where the command goes (R3, as dotnetcqrs's mapper): an automation on
	// its trigger's own aggregate acts on the trigger's own stream (no
	// prefix; with one, auto-ship sent ShipOrder to a stream that never
	// exists). Across aggregates the target id is derived from the source,
	// and a create is keyed by the triggering EVENT, so every trigger event
	// creates its own target, not just the first per source stream.
	prefix := ""
	if crossAggregate {
		prefix = DeriveID(TypeName(s.Name, s.ID)) + "-"
	}
	m.domain(source).Reactors = append(m.domain(source).Reactors, scaffold.Reactor{
		Name:      LowerFirst(scaffold.SanitizeName(TypeName(s.Name, s.ID))),
		On:        triggers,
		Aggregate: agg,
		Command:   m.commandName(s.CommandID),
		IDPrefix:  prefix,
		PerEvent:  isCreate,
	})
}

// command builds one scaffold command and its events, folding field types
// and reporting every fold that loses something.
func (m *mapper) command(agg, id string, c Command, eventIDs []string, once, requiresExisting bool) scaffold.Command {
	out := scaffold.Command{
		Name:             m.commandName(id),
		Fields:           m.fields("command "+id, c.Fields),
		Once:             once,
		RequiresExisting: requiresExisting,
		Refusals:         m.mapRefusals(id, c.RefuseWhen),
		OwnedBy:          m.mapStateOwnership(agg, id, c.RequiredOwnership),
	}
	for _, evID := range eventIDs {
		ev := m.doc.Events[evID]
		m.eventAggregate[evID] = agg
		e := scaffold.Event{Name: m.eventType(evID), EndsStream: ev.EndsStream}
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
		sf := scaffold.Field{
			Name:       scaffold.SanitizeName(f.Name),
			Type:       FoldType(f),
			Derivation: m.mapDerivation(f.Derivation),
			PII:        f.PII,
		}
		if f.PII {
			sf.PIISubject = scaffold.SanitizeName(f.PIISubject)
		}
		if f.Derivation != nil && f.Derivation.Kind == DerivationGroupBy {
			sf.Subfields = m.groupBySubfields(owner, f)
		}
		out = append(out, sf)
	}
	return out
}

// groupBySubfields maps a groupBy field's own nested subfields — the shape
// of one row in the parent field's list. A toggle subfield is rejected with
// a clear mapping error rather than silently generating wrong code,
// mirroring dotnetcqrs's DocumentMapper: ToggleDerivation carries no
// rowKeyField at all (schema-enforced — it is only ever meaningful
// same-stream), so there is no way to know which TOP-LEVEL row a toggle
// subfield's triggering event should update once nested inside a groupBy
// field, which is foreign-stream by construction (that is the whole reason
// groupBy exists). The field named by the parent's groupByField itself
// carries no derivation — its value is just the grouping key, copied
// straight from the matching event payload — so it passes through here
// exactly like any other plain field.
func (m *mapper) groupBySubfields(owner string, f Field) []scaffold.Field {
	out := make([]scaffold.Field, 0, len(f.Subfields))
	for _, sf := range f.Subfields {
		if sf.Derivation != nil && sf.Derivation.Kind == DerivationToggle {
			m.rep.errorf("%s: field %q groupBy subfield %q declares a toggle derivation, "+
				"which is not supported inside groupBy — only count/sum subfields are",
				owner, f.Name, sf.Name)
			continue
		}
		if note := FoldNote(owner, sf); note != "" {
			m.rep.warnf("%s", note)
		}
		out = append(out, scaffold.Field{
			Name:       scaffold.SanitizeName(sf.Name),
			Type:       FoldType(sf),
			Derivation: m.mapDerivation(sf.Derivation),
		})
	}
	return out
}

// mapDerivation carries a field's derivation through, resolving every event
// id reference to the generated event TYPE name (m.eventType) rather than a
// bare sanitized id — that's what the switch cases in deciderGo/
// projectionGo actually match on, exactly as `On`/eventIDs are resolved
// everywhere else in this file (see mapReadModels' `on` build).
func (m *mapper) mapDerivation(in *FieldDerivation) *scaffold.FieldDerivation {
	if in == nil {
		return nil
	}
	types := func(ids []string) []string {
		if len(ids) == 0 {
			return nil
		}
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = m.eventType(id)
		}
		return out
	}
	return &scaffold.FieldDerivation{
		Kind:                in.Kind,
		OnEventIDs:          types(in.OnEventIDs),
		OffEventIDs:         types(in.OffEventIDs),
		Initial:             in.Initial,
		IncrementOnEventIDs: types(in.IncrementOnEventIDs),
		DecrementOnEventIDs: types(in.DecrementOnEventIDs),
		AddOnEventIDs:       types(in.AddOnEventIDs),
		SubtractOnEventIDs:  types(in.SubtractOnEventIDs),
		AmountField:         in.AmountField,
		RowKeyField:         scaffold.SanitizeName(in.RowKeyField),
		GroupByField:        scaffold.SanitizeName(in.GroupByField),
	}
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
			Collection:   scaffold.SanitizeName(ReadModelCollectionName(rm.Name, id)),
			Key:          key,
			Fields:       m.fields("read model "+id, rm.Fields),
			On:           on,
			Scopes:       m.mapScopes(id, rm),
			Filters:      m.mapFilters(id, rm),
			RequiredRole: append([]string(nil), rm.RequiredRole...),
			SelfAccess:   m.mapSelfAccess(id, rm),
			RemovedBy:    m.mapRemovals(id, rm),
			RowKeyField:  m.mapRowKeyField(id, rm),
		})
	}
}

// mapScopes carries a read model's scopes through, resolving each
// via.readModelId to its OWN generated collection name (see mapReadModels'
// own `ReadModelCollectionName(rm.Name, id)` call) — the generator has no document to
// look the id up in at codegen time, so the mapper is where this reference
// must be resolved, exactly like eventType resolves an event id to its
// generated type name.
func (m *mapper) mapScopes(id string, rm ReadModel) []scaffold.ReadModelScope {
	if len(rm.Scopes) == 0 {
		return nil
	}
	out := make([]scaffold.ReadModelScope, 0, len(rm.Scopes))
	for _, sc := range rm.Scopes {
		via, ok := m.doc.ReadModels[sc.Via.ReadModelID]
		if !ok {
			m.rep.errorf("read model %q: scope %q names via read model %q, which does not exist",
				id, sc.Param, sc.Via.ReadModelID)
			continue
		}
		if sc.GrantsAccess && !hasField(rm.Fields, sc.Via.FilterLocalField) {
			m.rep.errorf("read model %q: granting scope %q filters on %q, which is not one of its fields",
				id, sc.Param, sc.Via.FilterLocalField)
			continue
		}
		out = append(out, scaffold.ReadModelScope{
			Param:        sc.Param,
			GrantsAccess: sc.GrantsAccess,
			Via: scaffold.ReadModelScopeVia{
				Collection:       scaffold.SanitizeName(ReadModelCollectionName(via.Name, sc.Via.ReadModelID)),
				MatchParamTo:     scaffold.SanitizeName(sc.Via.MatchParamTo),
				SelectField:      scaffold.SanitizeName(sc.Via.SelectField),
				FilterLocalField: scaffold.SanitizeName(sc.Via.FilterLocalField),
			},
		})
	}
	return out
}

// mapStateOwnership carries requiredOwnership.stateField (schema 3.15.0)
// through, with dotnetcqrs's default message when none is declared.
func (m *mapper) mapStateOwnership(agg, id string, own *CommandOwnership) *scaffold.Ownership {
	if own == nil || own.StateField == "" {
		return nil
	}
	field := scaffold.SanitizeName(own.StateField)
	msg := own.Message
	if msg == "" {
		msg = fmt.Sprintf("only this %s's own %s may %s", agg, field, m.commandName(id))
	}
	return &scaffold.Ownership{StateField: field, Message: msg}
}

// mapRefusals carries a command's refuseWhen rules through, field names
// sanitised as state and payload fields are. Whether each field exists and
// can be compared that way is checked by the generator (scaffold
// Domain.Validate), once the aggregate's events are known.
func (m *mapper) mapRefusals(id string, rules []CommandRefusal) []scaffold.Refusal {
	var out []scaffold.Refusal
	for _, rule := range rules {
		r := scaffold.Refusal{Message: rule.Message}
		for _, c := range rule.All {
			cond, err := mapRefusalCondition(c)
			if err != nil {
				m.rep.errorf("command %q: refusal %q: %v", id, rule.Message, err)
				continue
			}
			r.All = append(r.All, cond)
		}
		out = append(out, r)
	}
	return out
}

func mapRefusalCondition(c RefusalCondition) (scaffold.RefusalCondition, error) {
	out := scaffold.RefusalCondition{Side: "state", Field: c.State}
	if c.Command != "" {
		out.Side, out.Field = "command", c.Command
	}
	if (c.State == "") == (c.Command == "") {
		return out, fmt.Errorf("a condition reads exactly one of state or command")
	}
	out.Field = scaffold.SanitizeName(out.Field)
	ops := []struct {
		name string
		raw  json.RawMessage
	}{{"equals", c.Equals}, {"notEquals", c.NotEquals}, {"contains", c.Contains}, {"lessThan", c.LessThan},
		{"atMost", c.AtMost}, {"greaterThan", c.GreaterThan}, {"atLeast", c.AtLeast}}
	found := 0
	if c.Is != "" {
		if c.Is != "set" && c.Is != "notSet" {
			return out, fmt.Errorf("is must be set or notSet, got %q", c.Is)
		}
		out.Op, found = c.Is, 1
	}
	for _, op := range ops {
		if len(op.raw) == 0 {
			continue
		}
		found++
		operand, err := refusalOperand(op.raw)
		if err != nil {
			return out, fmt.Errorf("%s: %v", op.name, err)
		}
		out.Op, out.Operand = op.name, operand
	}
	if found != 1 {
		return out, fmt.Errorf("a condition applies exactly one operator, found %d", found)
	}
	return out, nil
}

// refusalOperand reads an operand: {"state"|"command": field}, or a string,
// number or bool literal.
func refusalOperand(raw json.RawMessage) (*scaffold.RefusalOperand, error) {
	var ref struct{ State, Command string }
	if json.Unmarshal(raw, &ref) == nil && (ref.State != "" || ref.Command != "") {
		if ref.State != "" && ref.Command != "" {
			return nil, fmt.Errorf("an operand names exactly one of state or command")
		}
		if ref.State != "" {
			return &scaffold.RefusalOperand{Side: "state", Field: scaffold.SanitizeName(ref.State)}, nil
		}
		return &scaffold.RefusalOperand{Side: "command", Field: scaffold.SanitizeName(ref.Command)}, nil
	}
	var lit any
	if err := json.Unmarshal(raw, &lit); err != nil {
		return nil, err
	}
	switch lit.(type) {
	case string, float64, bool:
		return &scaffold.RefusalOperand{Literal: lit}, nil
	}
	return nil, fmt.Errorf("an operand is a string, number, bool, or {state|command: field}")
}

// mapRowKeyField carries rowKeyField (schema 3.14.0) through. Every event
// the read model is built from or removed by must carry it: one that
// doesn't could name no row.
func (m *mapper) mapRowKeyField(id string, rm ReadModel) string {
	if rm.RowKeyField == "" {
		return ""
	}
	for _, evID := range append(append([]string{}, rm.BuiltFromEventIDs...), rm.RemovedByEventIDs...) {
		if !hasField(m.doc.Events[evID].Fields, rm.RowKeyField) {
			m.rep.errorf("read model %q: rowKeyField %q is not a field of event %q", id, rm.RowKeyField, evID)
		}
	}
	return scaffold.SanitizeName(rm.RowKeyField)
}

// mapRemovals carries removedByEventIds through as event types. Each must
// be one the read model is built from: a removal it never receives would
// leave the row behind, the stale grant the schema added this to prevent.
func (m *mapper) mapRemovals(id string, rm ReadModel) []string {
	var out []string
	for _, ev := range rm.RemovedByEventIDs {
		built := false
		for _, b := range rm.BuiltFromEventIDs {
			built = built || b == ev
		}
		if !built {
			m.rep.errorf("read model %q: removedByEventIds names %q, which is not in its builtFromEventIds", id, ev)
			continue
		}
		out = append(out, m.eventType(ev))
	}
	return out
}

// mapSelfAccess carries selfAccess through. Its subject field must be one
// of the read model's own fields: a rule naming a column that isn't there
// would match nothing, or the wrong thing.
func (m *mapper) mapSelfAccess(id string, rm ReadModel) *scaffold.SelfAccess {
	sa := rm.SelfAccess
	if sa == nil {
		return nil
	}
	if !hasField(rm.Fields, sa.SubjectField) {
		m.rep.errorf("read model %q: selfAccess.subjectField %q is not one of its fields", id, sa.SubjectField)
		return nil
	}
	return &scaffold.SelfAccess{SubjectField: scaffold.SanitizeName(sa.SubjectField), Param: sa.Param}
}

func hasField(fields []Field, name string) bool {
	for _, f := range fields {
		if f.Name == name {
			return true
		}
	}
	return false
}

// mapFilters carries a read model's declared query filters through,
// resolving each Field reference against the read model's own columns —
// this mapper has the document's field list to hand; the generated code
// does not, so the reference must be resolved here, the same reasoning
// mapScopes uses for a via read model's id. Rejects an unknown filter kind,
// mirroring how a groupBy toggle subfield is rejected here rather than left
// to fail deep inside generation or verification.
//
// A "match" filter (schema 3.1.0) is checked here but not carried through:
// search is not generated yet, so it is reported as lossy, by name. Dropping
// it is safe for a pii field too — the risk search carries is an index that
// reveals too much, and no index means nothing is revealed.
func (m *mapper) mapFilters(id string, rm ReadModel) []scaffold.ReadModelFilter {
	if len(rm.Filters) == 0 {
		return nil
	}
	fieldNames := map[string]bool{}
	byName := map[string]Field{}
	for _, f := range rm.Fields {
		fieldNames[scaffold.SanitizeName(f.Name)] = true
		byName[scaffold.SanitizeName(f.Name)] = f
	}
	out := make([]scaffold.ReadModelFilter, 0, len(rm.Filters))
	for _, filt := range rm.Filters {
		if filt.Kind == FilterMatch {
			if mf, ok := m.checkMatchFilter(id, filt, byName); ok {
				out = append(out, mf)
			}
			continue
		}
		if filt.Kind != FilterDateRange {
			m.rep.errorf("read model %q: filter %q has kind %q; only %q and %q are supported",
				id, filt.Param, filt.Kind, FilterDateRange, FilterMatch)
			continue
		}
		field := scaffold.SanitizeName(filt.Field)
		if !fieldNames[field] {
			m.rep.errorf("read model %q: filter %q names field %q, which is not one of this read model's own fields",
				id, filt.Param, filt.Field)
			continue
		}
		if len(filt.Presets) == 0 {
			m.rep.errorf("read model %q: filter %q declares no presets", id, filt.Param)
			continue
		}
		out = append(out, scaffold.ReadModelFilter{
			Param:   filt.Param,
			Field:   field,
			Kind:    filt.Kind,
			Presets: append([]string(nil), filt.Presets...),
		})
	}
	return out
}

// checkMatchFilter applies the reference and shape rules the schema leaves to
// a generator (3.1.0: "not enforced here"), the same rules dotnetcqrs's
// mapper enforces, so a document that imports on one stack imports on the
// other. It then reports the filter lossy: nothing here generates search yet.
func (m *mapper) checkMatchFilter(id string, filt ReadModelFilter, fields map[string]Field) (scaffold.ReadModelFilter, bool) {
	f, ok := fields[scaffold.SanitizeName(filt.Field)]
	if !ok {
		m.rep.errorf("read model %q: filter %q names field %q, which is not one of this read model's own fields",
			id, filt.Param, filt.Field)
		return scaffold.ReadModelFilter{}, false
	}
	bad := false
	switch filt.Mode {
	case MatchModeExact, MatchModePrefix, MatchModeContains:
	default:
		m.rep.errorf("read model %q: match filter %q has mode %q; want %q, %q or %q",
			id, filt.Param, filt.Mode, MatchModeExact, MatchModePrefix, MatchModeContains)
		bad = true
	}
	switch filt.Normalize {
	case "", NormalizeNone, NormalizeCaseFold, NormalizeEmail, NormalizePhone, NormalizePersonName:
	default:
		m.rep.errorf("read model %q: match filter %q has normalize %q, which is not one the schema defines",
			id, filt.Param, filt.Normalize)
		bad = true
	}
	if filt.MinPrefixLength != nil && filt.Mode != MatchModePrefix {
		m.rep.errorf("read model %q: match filter %q sets minPrefixLength, which only applies to mode %q",
			id, filt.Param, MatchModePrefix)
		bad = true
	}
	if f.Derivation != nil {
		m.rep.errorf("read model %q: match filter %q is over %q, a derived field; only a copied field can be searched",
			id, filt.Param, filt.Field)
		bad = true
	} else if FoldType(f) != "text" {
		m.rep.errorf("read model %q: match filter %q is over %q, which is not a text field",
			id, filt.Param, filt.Field)
		bad = true
	}
	if f.PII && filt.Mode == MatchModePrefix && filt.MinPrefixLength == nil {
		m.rep.errorf("read model %q: match filter %q is a prefix search over personal data and must declare minPrefixLength",
			id, filt.Param)
		bad = true
	}
	if bad {
		return scaffold.ReadModelFilter{}, false
	}
	if f.PII && filt.Mode == MatchModePrefix {
		m.rep.warnf("read model %q: match filter %q is a prefix search over personal data: anyone who can call the key "+
			"service's hmac endpoint can confirm a guessed first %d characters, then extend the guess one at a time — "+
			"prefix is only as private as access to hmac is", id, filt.Param, *filt.MinPrefixLength)
	}
	if f.PII && filt.Mode == MatchModeContains {
		m.rep.warnf("read model %q: match filter %q (contains over personal data) keeps a normalized PLAINTEXT index at "+
			"rest in search.db; that file must be excluded from backups (it is rebuilt from the log)", id, filt.Param)
	}
	out := scaffold.ReadModelFilter{Param: filt.Param, Field: scaffold.SanitizeName(filt.Field), Kind: FilterMatch,
		Mode: filt.Mode, Normalize: filt.Normalize}
	if filt.MinPrefixLength != nil {
		out.MinPrefixLength = *filt.MinPrefixLength
	} else if filt.Mode == MatchModePrefix {
		out.MinPrefixLength = 3 // the schema's default
	}
	// two hashed prefix filters on one field and normalizer share one index,
	// so they must agree on where prefixes start
	key := id + "|" + out.Field + "|" + out.Normalize
	if f.PII && filt.Mode == MatchModePrefix {
		if m.prefixMin == nil {
			m.prefixMin = map[string]int{}
		}
		if prev, seen := m.prefixMin[key]; seen && prev != out.MinPrefixLength {
			m.rep.errorf("read model %q: match filters on %q share one hashed prefix index but declare minPrefixLength %d and %d",
				id, filt.Field, prev, out.MinPrefixLength)
			return scaffold.ReadModelFilter{}, false
		}
		m.prefixMin[key] = out.MinPrefixLength
	}
	return out, true
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

// ReadModelCollectionName renders a read-model name as the collection
// identifier its generated projection uses -- exported so package authorize
// can resolve the SAME name a command's requiredOwnership/scope via
// read-model reference names, without duplicating this convention.
func ReadModelCollectionName(name, id string) string {
	return LowerFirst(TypeName(name, id))
}

// scenarioNetExists folds a scenario's `given` sequence into a net "does the
// stream currently exist" verdict, tracking a synthesized `exists` exactly
// as the generated decider's own Evolve will: a `given` event on the
// slice's OWN aggregate stream sets `exists` true, unless it is tagged
// `endsStream`, which resets it false; a foreign-aggregate given (an
// ordinary cross-aggregate precondition, e.g. "the project exists") is not
// evidence either way and is skipped. Order matters — this is a FOLD, not a
// scan for presence — because a `given` that assigns then unassigns nets
// back to "does not exist", which is create evidence, not update evidence
// (Finding 3 / proposal Q5): scanning for "any own-stream event present"
// would misclassify that reassignment case as update evidence and wrongly
// turn off the create-only guard.
func (m *mapper) scenarioNetExists(sc Scenario, agg string) bool {
	exists := false
	for _, g := range sc.Given {
		if m.eventOwners[g.EventID] != agg {
			continue
		}
		exists = !m.eventEndsStream[g.EventID]
	}
	return exists
}

// hasCreateEvidence reports whether a scenario proves this command opens a
// fresh stream: its `given` nets to "does not exist" under scenarioNetExists.
// An empty `given` is the obvious case (nets to false trivially); a `given`
// ending on an `endsStream` event on the own stream is the reassign case —
// also nets to false, also create evidence. Where no scenario qualifies
// there is no create evidence. Shared by a directly-invoked command
// (mapStateChange) and an automation's dispatched command (mapAutomation).
func (m *mapper) hasCreateEvidence(s Slice, agg string) bool {
	for _, sc := range s.Scenarios {
		if sc.Kind != KindStateChange {
			continue
		}
		if !m.scenarioNetExists(sc, agg) {
			return true
		}
	}
	return false
}

// hasBeginAgainEvidence reports whether a create may begin its stream again
// after an endsStream event: one of its slice's stateChange scenarios does
// exactly that, its `given` ending the own stream (netting to "does not
// exist" with an own-stream endsStream event in it) and the command still
// succeeding (a re-assign after an unassign). Error scenarios are not
// evidence. With no such scenario the end is final (D28): the generated
// create refuses an ended stream. Mirrors dotnetcqrs's
// HasBeginAgainEvidence.
func (m *mapper) hasBeginAgainEvidence(s Slice, agg string) bool {
	for _, sc := range s.Scenarios {
		if sc.Kind != KindStateChange || m.scenarioNetExists(sc, agg) {
			continue
		}
		for _, g := range sc.Given {
			if m.eventEndsStream[g.EventID] && m.eventOwners[g.EventID] == agg {
				return true
			}
		}
	}
	return false
}

// hasUpdateEvidence is the mirror of hasCreateEvidence: a scenario proves
// this command can run against an ALREADY-existing stream when its `given`
// nets to "exists" under scenarioNetExists. A command with BOTH kinds of
// evidence is a genuine upsert (see mapStateChange) — neither strictly
// create-only nor strictly requires-existing, so neither guard applies.
func (m *mapper) hasUpdateEvidence(s Slice, agg string) bool {
	for _, sc := range s.Scenarios {
		if sc.Kind != KindStateChange {
			continue
		}
		if m.scenarioNetExists(sc, agg) {
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
				if scaffold.SanitizeName(ReadModelCollectionName(srm.Name, id)) == rm.Collection {
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
