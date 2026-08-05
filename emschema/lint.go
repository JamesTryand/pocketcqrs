package emschema

import (
	"fmt"
	"sort"
	"strings"
)

// Report collects everything an import decided or could not decide.
//
// It is the whole answer to this project's recurring defect: every mapping
// choice taken on a document's behalf — an invented aggregate, a folded
// field type, a dropped PII flag, a synthesized swimlane — is named here, or
// it is a silent wrong-doing. A count is not enough on its own; the entry
// says what was decided and why.
type Report struct {
	// Errors make the document unimportable.
	Errors []string
	// Warnings are decisions taken on the document's behalf, and gaps the
	// author still has to close. Import proceeds.
	Warnings []string
	// Lossy names what this project cannot represent at all.
	Lossy []string
}

// Err returns a single error describing every problem at once, or nil.
func (r *Report) Err() error {
	if len(r.Errors) == 0 {
		return nil
	}
	return fmt.Errorf("emschema: %s", strings.Join(r.Errors, "; "))
}

func (r *Report) errorf(format string, args ...any) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
}

func (r *Report) warnf(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

func (r *Report) lossyf(format string, args ...any) {
	r.Lossy = append(r.Lossy, fmt.Sprintf(format, args...))
}

// Lint checks a document for everything the source schema deliberately does
// not: referential integrity, pattern-field legality, and whether the
// document is from a schema generation this code understands.
//
// The source schema is structural only — JSON Schema has no cross-property
// lookup — so nothing upstream verifies that an id resolves. Without this,
// a dangling readModelId would surface as a confusing failure deep inside
// generation instead of as "slice X references read model Y, which does not
// exist".
func Lint(doc *Document) *Report {
	r := &Report{}
	checkGeneration(doc, r)

	swimlanes := map[string]bool{}
	for _, s := range doc.Swimlanes {
		if swimlanes[s.ID] {
			r.errorf("swimlane id %q is declared twice", s.ID)
		}
		swimlanes[s.ID] = true
	}
	if len(doc.Swimlanes) == 0 {
		r.errorf("a document needs at least one swimlane")
	}

	// every EVENT carries a swimlaneId too, not just every slice — a fact
	// worth remembering when synthesizing one on export
	for id, e := range doc.Events {
		if e.SwimlaneID == "" {
			r.errorf("event %q has no swimlaneId", id)
		} else if !swimlanes[e.SwimlaneID] {
			r.errorf("event %q references swimlane %q, which does not exist", id, e.SwimlaneID)
		}
	}

	sliceIDs := map[string]bool{}
	for _, s := range doc.Slices {
		if sliceIDs[s.ID] {
			r.errorf("slice id %q is declared twice", s.ID)
		}
		sliceIDs[s.ID] = true
		lintSlice(doc, s, swimlanes, r)
	}

	for id, rm := range doc.ReadModels {
		for _, evID := range rm.BuiltFromEventIDs {
			if _, ok := doc.Events[evID]; !ok {
				r.errorf("read model %q is built from event %q, which does not exist", id, evID)
			}
		}
	}

	notePII(doc, r)
	noteLossy(doc, r)
	sort.Strings(r.Errors)
	return r
}

// checkGeneration works out whether this document is from a schema
// generation this code understands — from its SHAPE, never its version
// string.
//
// The version cannot be trusted: v2 removed enum values, which is breaking,
// while both worked examples still declare "1.0.0" and the schema's own
// default is still "1.0.0". So a document declaring 1.0.0 may be a v1
// document that no longer validates, or a v2 one.
func checkGeneration(doc *Document, r *Report) {
	for _, s := range doc.Slices {
		switch s.Pattern {
		case PatternStateChange, PatternStateView, PatternAutomation:
		case "translation":
			r.errorf("slice %q uses the `translation` pattern, which was REMOVED in schema v2: "+
				"a read model never originates an event, so translation collapses into `automation` "+
				"(event(s) -> command -> event(s)). Re-express it as an automation slice; "+
				"see the schema project's docs/design-notes.md, \"v2: translation removed\"", s.ID)
		default:
			r.errorf("slice %q has unknown pattern %q", s.ID, s.Pattern)
		}
	}
}

// lintSlice checks one slice's references and that it carries only the
// fields its pattern allows.
//
// The legality half matters as much as the reference half: the source schema
// is allOf + if/then under a single unevaluatedProperties:false, so a
// property is legal ONLY on the pattern whose branch declares it. A
// screenId on an automation slice is not "extra", it is invalid — which is
// the same trap an export must avoid when synthesizing.
func lintSlice(doc *Document, s Slice, swimlanes map[string]bool, r *Report) {
	if s.SwimlaneID == "" {
		r.errorf("slice %q has no swimlaneId", s.ID)
	} else if !swimlanes[s.SwimlaneID] {
		r.errorf("slice %q references swimlane %q, which does not exist", s.ID, s.SwimlaneID)
	}
	if s.ChapterID != "" {
		if _, ok := doc.Chapters[s.ChapterID]; !ok {
			r.errorf("slice %q references chapter %q, which does not exist", s.ID, s.ChapterID)
		}
	}
	requireCommand := func(id string) {
		if id == "" {
			r.errorf("slice %q (%s) has no commandId", s.ID, s.Pattern)
			return
		}
		if _, ok := doc.Commands[id]; !ok {
			r.errorf("slice %q references command %q, which does not exist", s.ID, id)
		}
	}
	requireEvents := func(label string, ids []string) {
		if len(ids) == 0 {
			r.errorf("slice %q (%s) lists no %s", s.ID, s.Pattern, label)
		}
		for _, id := range ids {
			if _, ok := doc.Events[id]; !ok {
				r.errorf("slice %q references event %q in %s, which does not exist", s.ID, id, label)
			}
		}
	}
	requireScreen := func() {
		if s.ScreenID == "" {
			r.errorf("slice %q (%s) has no screenId", s.ID, s.Pattern)
			return
		}
		if _, ok := doc.Screens[s.ScreenID]; !ok {
			r.errorf("slice %q references screen %q, which does not exist", s.ID, s.ScreenID)
		}
	}
	requireReadModel := func() {
		if s.ReadModelID == "" {
			r.errorf("slice %q (%s) has no readModelId", s.ID, s.Pattern)
			return
		}
		if _, ok := doc.ReadModels[s.ReadModelID]; !ok {
			r.errorf("slice %q references read model %q, which does not exist", s.ID, s.ReadModelID)
		}
	}
	forbid := func(field string, present bool) {
		if present {
			r.errorf("slice %q (%s) carries %s, which the %s pattern does not declare — "+
				"the source schema's unevaluatedProperties would reject this document",
				s.ID, s.Pattern, field, s.Pattern)
		}
	}

	switch s.Pattern {
	case PatternStateChange:
		requireScreen()
		requireCommand(s.CommandID)
		requireEvents("eventIds", s.EventIDs)
		forbid("readModelId", s.ReadModelID != "")
		forbid("automationId", s.AutomationID != "")
		forbid("triggerEventIds", len(s.TriggerEventIDs) > 0)
		forbid("resultEventIds", len(s.ResultEventIDs) > 0)
	case PatternStateView:
		requireScreen()
		requireReadModel()
		forbid("commandId", s.CommandID != "")
		forbid("eventIds", len(s.EventIDs) > 0)
		forbid("automationId", s.AutomationID != "")
		forbid("triggerEventIds", len(s.TriggerEventIDs) > 0)
		forbid("resultEventIds", len(s.ResultEventIDs) > 0)
	case PatternAutomation:
		if s.AutomationID == "" {
			r.errorf("slice %q (automation) has no automationId", s.ID)
		} else if _, ok := doc.Automations[s.AutomationID]; !ok {
			r.errorf("slice %q references automation %q, which does not exist", s.ID, s.AutomationID)
		}
		requireEvents("triggerEventIds", s.TriggerEventIDs)
		requireCommand(s.CommandID)
		requireEvents("resultEventIds", s.ResultEventIDs)
		// readModelId is OPTIONAL here: v2 made it so, because many real
		// automations (boundary-crossing "Bridge" ones) are stateless
		if s.ReadModelID != "" {
			requireReadModel()
		}
		forbid("screenId", s.ScreenID != "")
		forbid("eventIds", len(s.EventIDs) > 0)
	}

	lintScenarios(doc, s, r)
}

// lintScenarios checks scenario references and that each kind is one the
// slice's pattern allows.
func lintScenarios(doc *Document, s Slice, r *Report) {
	allowed := map[string]bool{}
	switch s.Pattern {
	case PatternStateChange, PatternAutomation:
		allowed[KindStateChange], allowed[KindError] = true, true
	case PatternStateView:
		allowed[KindStateView] = true
	}
	for _, sc := range s.Scenarios {
		if !allowed[sc.Kind] {
			r.errorf("slice %q (%s) has a %q scenario, which that pattern does not allow",
				s.ID, s.Pattern, sc.Kind)
		}
		for _, g := range sc.Given {
			if _, ok := doc.Events[g.EventID]; !ok {
				r.errorf("scenario %q gives event %q, which does not exist", sc.ID, g.EventID)
			}
		}
		switch sc.Kind {
		case KindStateChange:
			if ref, err := sc.CommandRef(); err != nil {
				r.errorf("scenario %q: %v", sc.ID, err)
			} else if _, ok := doc.Commands[ref.CommandID]; !ok {
				r.errorf("scenario %q uses command %q, which does not exist", sc.ID, ref.CommandID)
			}
			then, err := sc.EventsThen()
			if err != nil {
				r.errorf("scenario %q: %v", sc.ID, err)
				continue
			}
			for _, e := range then.Events {
				if _, ok := doc.Events[e.EventID]; !ok {
					r.errorf("scenario %q expects event %q, which does not exist", sc.ID, e.EventID)
				}
			}
		case KindError:
			if ref, err := sc.CommandRef(); err != nil {
				r.errorf("scenario %q: %v", sc.ID, err)
			} else if _, ok := doc.Commands[ref.CommandID]; !ok {
				r.errorf("scenario %q uses command %q, which does not exist", sc.ID, ref.CommandID)
			}
			if _, err := sc.ErrorThen(); err != nil {
				r.errorf("scenario %q: %v", sc.ID, err)
			}
		case KindStateView:
			q, err := sc.ReadModelQuery()
			if err != nil {
				r.errorf("scenario %q: %v", sc.ID, err)
				continue
			}
			if _, ok := doc.ReadModels[q.ReadModelID]; !ok {
				r.errorf("scenario %q queries read model %q, which does not exist", sc.ID, q.ReadModelID)
			}
		}
	}
}

// notePII records PII flags that this project cannot carry.
//
// A PII flag silently dropped is the worst kind of loss this format can
// suffer, because the flag exists precisely so a generator can treat the
// field carefully. //@schema has no place for it, so the honest answer is to
// name every one.
func notePII(doc *Document, r *Report) {
	var flagged []string
	walk := func(owner string, fields []Field) {
		var recurse func(prefix string, fs []Field)
		recurse = func(prefix string, fs []Field) {
			for _, f := range fs {
				if f.PII {
					flagged = append(flagged, owner+"."+prefix+f.Name)
				}
				recurse(prefix+f.Name+".", f.Subfields)
			}
		}
		recurse("", fields)
	}
	for id, e := range doc.Events {
		walk("event "+id, e.Fields)
	}
	for id, c := range doc.Commands {
		walk("command "+id, c.Fields)
	}
	for id, rm := range doc.ReadModels {
		walk("read model "+id, rm.Fields)
	}
	if len(flagged) > 0 {
		sort.Strings(flagged)
		r.lossyf("%d field(s) are marked pii and NOTHING here carries that flag: %s. "+
			"They are stored as ordinary columns; treat them accordingly",
			len(flagged), strings.Join(flagged, ", "))
	}
}

// noteLossy enumerates what this project has no concept of, so the list is
// in the output rather than in someone's memory.
func noteLossy(doc *Document, r *Report) {
	if len(doc.Chapters) > 0 {
		r.lossyf("%d chapter(s) are design-time grouping with no runtime concept here", len(doc.Chapters))
	}
	if len(doc.ActorLanes) > 0 {
		r.lossyf("%d actor lane(s) are design-time notation with no runtime concept here", len(doc.ActorLanes))
	}
	if len(doc.Hotspots) > 0 {
		r.lossyf("%d hotspot(s) are open questions about the model; they are carried into the domain doc, not into code", len(doc.Hotspots))
	}
	if len(doc.Screens) > 0 {
		r.lossyf("%d screen(s) have no runtime concept here; an export synthesizes them back", len(doc.Screens))
	}
	statuses := map[string]int{}
	for _, s := range doc.Slices {
		if s.Status != "" {
			statuses[s.Status]++
		}
	}
	if len(statuses) > 0 {
		r.lossyf("slice status is board state and is not preserved (%s)", countsList(statuses))
	}
	var optional int
	for _, e := range doc.Events {
		for _, f := range e.Fields {
			if f.Optional {
				optional++
			}
		}
	}
	if optional > 0 {
		r.lossyf("%d field(s) are marked optional; //@schema cannot express nullability yet, "+
			"so they become ordinary columns", optional)
	}
}

func countsList(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s×%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}
