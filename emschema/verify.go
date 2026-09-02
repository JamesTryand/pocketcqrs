package emschema

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
	"github.com/jamestryand/pocketcqrs/scaffold"
)

// nowFunc is time.Now, overridable in tests so last7Days/lastCalendarMonth
// resolve against a fixed reference instant instead of the real clock.
var nowFunc = time.Now

// scenarioStream is the aggregate id every scenario runs against.
//
// A scenario describes one instance's story — "given these events, when this
// command" — without naming an id, so one is fixed here. Using the same id
// for every scenario is safe because each runs against its own scratch store.
const scenarioStream = "scenario"

// ScenarioResult is one scenario checked against the generated code.
type ScenarioResult struct {
	SliceID    string
	ScenarioID string
	Name       string
	Kind       string
	Passed     bool
	Skipped    bool
	// Detail says what happened: why it failed, or why it was skipped.
	Detail string
}

// Verify runs a document's scenarios against the code the import generated.
//
// This is what turns a scenario from documentation into a check. A document
// says "given these events, when this command, then these events" — which is
// exactly the shape of a decide dry run — so an import can tell the operator
// whether the code it just generated actually behaves as the model claims,
// before any of it is saved.
//
// Nothing is appended and no decider is registered: every run happens in a
// scratch store under dir, against source held in memory.
//
// A FAILING scenario is not an import error. The generated decider is a
// starting point whose rules are the author's job, so a scenario failing is
// usually the document describing behaviour nobody has written yet — the
// useful output is the list, not a verdict.
func Verify(doc *Document, mapped *Mapped, dir string) ([]ScenarioResult, error) {
	v := &verifier{doc: doc, dir: dir, sources: map[string]scaffold.File{}}
	if err := v.indexSources(mapped); err != nil {
		return nil, err
	}

	var out []ScenarioResult
	for _, s := range doc.Slices {
		for _, sc := range s.Scenarios {
			out = append(out, v.run(s, sc))
		}
	}
	return out, nil
}

type verifier struct {
	doc *Document
	dir string
	// sources maps a generated file name to its content, so a scenario can
	// find the decider or projection it needs to exercise.
	sources map[string]scaffold.File
	// aggregateOf maps a command id to the aggregate that owns it.
	aggregateOf map[string]string
	// collectionOf maps a read model id to its generated collection.
	collectionOf map[string]string
	// eventAggregate maps a generated event TYPE to the aggregate whose
	// decider handles it, so a scenario's `given` can be sorted into the
	// stream it belongs to.
	eventAggregate map[string]string
}

func (v *verifier) indexSources(mapped *Mapped) error {
	v.aggregateOf = map[string]string{}
	v.collectionOf = map[string]string{}
	v.eventAggregate = map[string]string{}
	for _, d := range mapped.Domains {
		files, err := d.Generate()
		if err != nil {
			return fmt.Errorf("generating %s for verification: %w", d.Aggregate, err)
		}
		for _, f := range files {
			v.sources[f.Name] = f
		}
		for _, c := range d.Commands {
			v.aggregateOf[c.Name] = d.Aggregate
		}
		for _, rm := range d.ReadModels {
			v.collectionOf[rm.Collection] = d.Aggregate
		}
		for _, t := range d.Events() {
			v.eventAggregate[t] = d.Aggregate
		}
	}
	return nil
}

// streamID picks the aggregate id a scenario runs against.
//
// `idAttribute: true` names the field carrying instance identity, so when the
// fixture supplies it the scenario runs against THAT id. It matters: a
// stateView scenario queries by the same value, and a fixed placeholder id
// would make every such query miss for a reason that has nothing to do with
// the projection under test.
func (v *verifier) streamID(given []EventRef) string {
	for _, g := range given {
		ev, ok := v.doc.Events[g.EventID]
		if !ok || len(g.Data) == 0 {
			continue
		}
		var data map[string]any
		if json.Unmarshal(g.Data, &data) != nil {
			continue
		}
		for _, f := range ev.Fields {
			if !f.IDAttribute {
				continue
			}
			if val, present := data[f.Name]; present {
				if str, isStr := val.(string); isStr && str != "" {
					return str
				}
			}
		}
	}
	return scenarioStream
}

func (v *verifier) run(s Slice, sc Scenario) ScenarioResult {
	res := ScenarioResult{SliceID: s.ID, ScenarioID: sc.ID, Name: sc.Name, Kind: sc.Kind}
	switch sc.Kind {
	case KindStateChange, KindError:
		v.runCommandScenario(s, sc, &res)
	case KindStateView:
		v.runViewScenario(s, sc, &res)
	default:
		res.Skipped = true
		res.Detail = "unknown scenario kind"
	}
	return res
}

// runCommandScenario covers stateChange and error: both are a command applied
// to a folded stream, differing only in what they assert about the answer.
//
// The error kind fits `mode=decide` exactly as directly as stateChange does —
// which is only true because a refusal is a VERDICT rather than an error. If
// a rejection were still a bare failure, "the decider correctly refused" and
// "the candidate is broken" would be the same result here.
func (v *verifier) runCommandScenario(s Slice, sc Scenario, res *ScenarioResult) {
	ref, err := sc.CommandRef()
	if err != nil {
		res.Detail = err.Error()
		return
	}
	cmdName := v.commandName(ref.CommandID)
	aggregate, ok := v.aggregateOf[cmdName]
	if !ok {
		res.Skipped = true
		res.Detail = fmt.Sprintf("command %q was not generated (its slice may have been skipped)", cmdName)
		return
	}
	file, ok := v.sources[aggregate+".js"]
	if !ok {
		res.Skipped = true
		res.Detail = fmt.Sprintf("no decider was generated for %q", aggregate)
		return
	}

	// `given` may name events from ANOTHER aggregate: an automation
	// scenario's history is the trigger event, which lives on the stream
	// that caused the reaction, not on the one the command targets. Seeding
	// those here would make the decider fold an event it does not handle —
	// a failure about the fixture, not about the domain.
	own, foreign := v.splitGiven(aggregate, sc.Given)
	stream := v.streamID(sc.Given)
	store, err := v.fixtureStore(sc.ID, aggregate, stream, own)
	if err != nil {
		res.Detail = err.Error()
		return
	}
	defer store.Close()

	rt := functions.NewGojaRuntime(nil)
	spec, err := functions.LoadDeciderSource(rt, aggregate+".js", file.Source)
	if err != nil {
		res.Detail = "the generated decider did not load: " + err.Error()
		return
	}
	payload := json.RawMessage(`{}`)
	if len(ref.Data) > 0 {
		payload = ref.Data
	}
	run, err := functions.DryRunDecide(store, spec, stream,
		decider.Command{Name: cmdName, Payload: payload},
		map[string]any{"now": "scenario", "actor": "schema-import"})
	if err != nil {
		res.Detail = "the dry run could not be answered: " + err.Error()
		return
	}

	if sc.Kind == KindError {
		then, terr := sc.ErrorThen()
		if terr != nil {
			res.Detail = terr.Error()
			return
		}
		if !run.Rejected {
			res.Detail = fmt.Sprintf("expected a refusal (%q) but the command was accepted and would append %d event(s)",
				then.Error.Message, len(run.Produced))
			return
		}
		res.Passed = true
		res.Detail = "refused: " + run.Message
		return
	}

	if run.Rejected {
		res.Detail = "the decider refused the command: " + run.Message
		return
	}
	then, terr := sc.EventsThen()
	if terr != nil {
		res.Detail = terr.Error()
		return
	}
	var want []string
	for _, e := range then.Events {
		want = append(want, v.eventType(e.EventID))
	}
	var got []string
	for _, e := range run.Produced {
		got = append(got, e.Type)
	}
	if strings.Join(want, ",") != strings.Join(got, ",") {
		res.Detail = fmt.Sprintf("expected [%s], got [%s]", strings.Join(want, ", "), strings.Join(got, ", "))
		return
	}
	// The EVENT TYPES are the assertion. Example data is reported but does
	// not fail the scenario, because the source schema is explicit that
	// nothing cross-checks a scenario's `data` against the declared fields
	// of the event it names — the two are allowed to diverge, and a document
	// that is correct by its own rules must not fail here. The divergence is
	// still worth naming: it is usually the generated decider not yet doing
	// what the model describes, which is exactly what an author needs told.
	res.Passed = true
	res.Detail = fmt.Sprintf("would append %s", strings.Join(got, ", "))
	var notes []string
	if len(foreign) > 0 {
		types := make([]string, 0, len(foreign))
		for _, g := range foreign {
			types = append(types, v.eventType(g.EventID))
		}
		notes = append(notes, fmt.Sprintf("%s belong to another aggregate and were treated as the "+
			"trigger rather than this stream's history", strings.Join(types, ", ")))
	}
	for i, e := range then.Events {
		if len(e.Data) == 0 {
			continue
		}
		if diff := subsetDiff(e.Data, run.Produced[i].Data); diff != "" {
			notes = append(notes, fmt.Sprintf("%s payload %s", got[i], diff))
		}
	}
	if len(notes) > 0 {
		res.Detail += " — but the example data differs: " + strings.Join(notes, "; ") +
			". The generated decider copies only the fields the EVENT declares, from the command payload " +
			"of the same name; anything else is a rule to write."
	}
}

// runViewScenario covers stateView: fold the fixture through the generated
// projection, then ask the resulting rows what the query asks.
//
// This is the two-step decided for stateView rather than a server-side
// `mode=view`: the projection run is isolated from live collections, and the
// query semantics live here, in the tool that knows what a scenario is.
func (v *verifier) runViewScenario(s Slice, sc Scenario, res *ScenarioResult) {
	q, err := sc.ReadModelQuery()
	if err != nil {
		res.Detail = err.Error()
		return
	}
	rm, ok := v.doc.ReadModels[q.ReadModelID]
	if !ok {
		res.Detail = fmt.Sprintf("read model %q does not exist", q.ReadModelID)
		return
	}
	collection := scaffold.SanitizeName(collectionName(rm.Name, q.ReadModelID))
	file, ok := v.sources[collection+".js"]
	if !ok {
		res.Skipped = true
		res.Detail = fmt.Sprintf("no projection was generated for %q", collection)
		return
	}

	rt := functions.NewGojaRuntime(nil)
	spec, err := functions.LoadProjectionSource(rt, nil, collection+".js", file.Source)
	if err != nil {
		res.Detail = "the generated projection did not load: " + err.Error()
		return
	}
	// Each given event needs its OWN row key for THIS projection, not one key
	// shared by the whole scenario (the pre-Finding-3 design): an unscoped
	// "manager sees every flagged entry" scenario deliberately seeds two
	// DIFFERENT entries and expects two DIFFERENT rows out, which one shared
	// synthesized id can never produce (every given event would collapse onto
	// the same row). See rowKey's doc comment for the resolution rule.
	sharedDefault := v.streamID(sc.Given)
	fixture := v.buildFixture(sc.Given, idAttributeField(rm), sharedDefault)
	run, err := functions.DryRunProjectionOver(spec, fixture)
	if err != nil {
		res.Detail = "the projection failed over the fixture: " + err.Error()
		return
	}
	then, terr := sc.ResultThen()
	if terr != nil {
		res.Detail = terr.Error()
		return
	}

	rows := run.Rows[collection]
	scopedRows, remainingParams, scopeErr := v.filterByScopes(rm, rows, q.QueryParams, sc.Given, sharedDefault)
	if scopeErr != "" {
		res.Detail = scopeErr
		return
	}
	filteredRows, remainingParams, filterErr := filterByFilters(rm, scopedRows, remainingParams)
	if filterErr != "" {
		res.Detail = filterErr
		return
	}
	matches := selectRows(filteredRows, remainingParams)

	// Two `then.result` shapes are supported. A single-property object whose
	// value is a JSON array (e.g. `{"entries": [...]}`) is a NAMED-LIST
	// result: the query may return any number of rows, and each element of
	// the array must match exactly one of them, with none left over on either
	// side -- this is the shape every real scenario in practice uses (a view
	// is a list of rows, even when there's only one). Anything else is a flat
	// single-row result (the original, pre-Finding-3 shape, still used by
	// this package's own order-fulfillment.json regression test): the query
	// must match EXACTLY one row, compared directly.
	if expectedRows, isList := tryUnwrapExpectedList(then.Result); isList {
		if diff := listDiff(expectedRows, matches); diff != "" {
			res.Detail = "result: " + diff
			return
		}
		res.Passed = true
		res.Detail = fmt.Sprintf("the projected rows match (%d)", len(matches))
		return
	}

	if len(matches) != 1 {
		if len(matches) == 0 {
			res.Detail = fmt.Sprintf("no row matched the query; the projection produced %d row(s)", len(rows))
		} else {
			res.Detail = fmt.Sprintf("the query is ambiguous: %d rows matched", len(matches))
		}
		return
	}
	encoded, _ := json.Marshal(matches[0])
	if diff := subsetDiff(then.Result, encoded); diff != "" {
		res.Detail = "result: " + diff
		return
	}
	res.Passed = true
	res.Detail = "the projected row matches"
}

// idAttributeField returns the field a read model itself declares
// `idAttribute: true` on — its own key/id column, as opposed to streamID's
// EVENT-level idAttribute (only present on an aggregate's own creation
// event).
func idAttributeField(rm ReadModel) string {
	for _, f := range rm.Fields {
		if f.IDAttribute {
			return f.Name
		}
	}
	return ""
}

// rowKey resolves one given event's row key for one target projection: if
// the event's own payload names that read model's own id field, use that
// value (lets two given events about two different entities land on two
// different rows); otherwise fall back to the scenario-wide default (keeps
// every given event on the SAME row when the scenario doesn't distinguish
// them — the common case, and the pre-Finding-3 behaviour every existing
// flat-result scenario already relies on).
func rowKey(idField string, data json.RawMessage, fallback string) string {
	if idField == "" || len(data) == 0 {
		return fallback
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return fallback
	}
	if val, ok := m[idField]; ok {
		if str, isStr := val.(string); isStr && str != "" {
			return str
		}
	}
	return fallback
}

// buildFixture folds a scenario's given events into event fixtures for ONE
// target projection, resolving each event's own AggregateID from that
// projection's read model's own id field where present (see rowKey) instead
// of one id shared by the whole scenario.
func (v *verifier) buildFixture(given []EventRef, idField, fallback string) []events.Event {
	fixture := make([]events.Event, 0, len(given))
	for i, g := range given {
		data := g.Data
		if len(data) == 0 {
			data = json.RawMessage(`{}`)
		}
		fixture = append(fixture, events.Event{
			Position: int64(i + 1), ID: fmt.Sprintf("scenario-%d", i+1),
			Aggregate: v.doc.Events[g.EventID].Aggregate, AggregateID: rowKey(idField, data, fallback),
			Sequence: int64(i + 1), Type: v.eventType(g.EventID), Data: data,
			Created: "1970-01-01 00:00:00.000Z",
		})
	}
	return fixture
}

// filterByScopes applies a read model's declared `scopes` (Finding 3,
// Addition C) to a stateView scenario's rows: a scoped query param resolves
// through another read model's own row (a semi-join) rather than matching a
// plain column, so it must be handled BEFORE selectRow's plain field-match
// runs — a param scopes declares has no matching column on this read model
// at all (e.g. `pmStaffId` on `flagged-entries`), so leaving it in the
// remaining params would make selectRow fail to find it and reject every
// row.
//
// The via read model is folded over the SAME given events as the target,
// through its own buildFixture call (so it gets its OWN row keys, per its
// own idAttribute field — see rowKey), and its rows carry whatever
// `matchParamTo`/`selectField` values the fixture's events gave them. This
// mirrors the real semi-join (`WHERE filterLocalField IN (SELECT selectField
// FROM via WHERE matchParamTo = :param)`) at verify-scenario grain: one via
// row set, checked against one candidate target row set.
func (v *verifier) filterByScopes(rm ReadModel, rows map[string]map[string]any, queryParams json.RawMessage, given []EventRef, sharedDefault string) (map[string]map[string]any, json.RawMessage, string) {
	if len(rm.Scopes) == 0 || len(queryParams) == 0 {
		return rows, queryParams, ""
	}
	var params map[string]any
	if err := json.Unmarshal(queryParams, &params); err != nil {
		return rows, queryParams, ""
	}
	remaining := map[string]any{}
	for k, val := range params {
		remaining[k] = val
	}

	filtered := rows
	for _, scope := range rm.Scopes {
		paramVal, present := params[scope.Param]
		if !present {
			continue
		}
		delete(remaining, scope.Param)

		via, ok := v.doc.ReadModels[scope.Via.ReadModelID]
		if !ok {
			return nil, nil, fmt.Sprintf("scope %q names via read model %q, which does not exist",
				scope.Param, scope.Via.ReadModelID)
		}
		viaCollection := scaffold.SanitizeName(collectionName(via.Name, scope.Via.ReadModelID))
		file, ok := v.sources[viaCollection+".js"]
		if !ok {
			return nil, nil, fmt.Sprintf("scope %q's via read model %q was not generated", scope.Param, viaCollection)
		}
		rt := functions.NewGojaRuntime(nil)
		spec, err := functions.LoadProjectionSource(rt, nil, viaCollection+".js", file.Source)
		if err != nil {
			return nil, nil, "the scope's via projection did not load: " + err.Error()
		}
		viaFixture := v.buildFixture(given, idAttributeField(via), sharedDefault)
		run, err := functions.DryRunProjectionOver(spec, viaFixture)
		if err != nil {
			return nil, nil, "the scope's via projection failed over the fixture: " + err.Error()
		}

		matchTo := scaffold.SanitizeName(scope.Via.MatchParamTo)
		selectField := scaffold.SanitizeName(scope.Via.SelectField)
		var allowed []any
		for _, viaRow := range run.Rows[viaCollection] {
			if jsonEq(viaRow[matchTo], paramVal) {
				allowed = append(allowed, viaRow[selectField])
			}
		}

		localField := scaffold.SanitizeName(scope.Via.FilterLocalField)
		next := map[string]map[string]any{}
		for key, row := range filtered {
			for _, av := range allowed {
				if jsonEq(row[localField], av) {
					next[key] = row
					break
				}
			}
		}
		filtered = next
	}

	encoded, _ := json.Marshal(remaining)
	return filtered, encoded, ""
}

// filterByFilters applies a read model's declared `filters` (schema 2.4.0,
// F-20's dateRange follow-up) to a stateView scenario's rows, mirroring
// filterByScopes' shape: a declared filter param has no matching column on
// this read model (its param name, e.g. `dateRange`, is not itself a field —
// the range applies to a DIFFERENT field, named by filt.Field), so it must
// be resolved and removed from queryParams BEFORE selectRows' plain
// field-equality match runs, exactly like a scope param.
//
// This is the direct Go-side fix for the gap Findings 1-3 confirmed in both
// the .NET harness and this one: a queryParams value that is a JSON object
// (a dateRange param's own {"kind": ...} shape) was previously deleted by
// selectRows rather than turned into a predicate, because there was no
// column or declared filter for it to become one against. A read model that
// declares the param in Filters now gets a real range predicate instead.
func filterByFilters(rm ReadModel, rows map[string]map[string]any, queryParams json.RawMessage) (map[string]map[string]any, json.RawMessage, string) {
	if len(rm.Filters) == 0 || len(queryParams) == 0 {
		return rows, queryParams, ""
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(queryParams, &params); err != nil {
		return rows, queryParams, ""
	}
	remaining := map[string]json.RawMessage{}
	for k, v := range params {
		remaining[k] = v
	}

	filtered := rows
	for _, filt := range rm.Filters {
		raw, present := params[filt.Param]
		if !present {
			continue
		}
		delete(remaining, filt.Param)

		if filt.Kind != FilterDateRange {
			return nil, nil, fmt.Sprintf("filter %q has kind %q, which this verifier does not know how to apply",
				filt.Param, filt.Kind)
		}
		var val map[string]any
		if err := json.Unmarshal(raw, &val); err != nil {
			return nil, nil, fmt.Sprintf(
				"filter %q's queryParams value must be an object like {\"kind\": \"last7Days\"}, got %s", filt.Param, raw)
		}
		preset, _ := val["kind"].(string)
		allowed := false
		for _, p := range filt.Presets {
			if p == preset {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, nil, fmt.Sprintf("filter %q: preset %q is not one of this filter's declared presets %v",
				filt.Param, preset, filt.Presets)
		}
		from, to, err := resolveDateRangeFilter(preset, val, nowFunc())
		if err != nil {
			return nil, nil, fmt.Sprintf("filter %q: %v", filt.Param, err)
		}

		next := map[string]map[string]any{}
		for key, row := range filtered {
			t, ok := parseFilterDate(row[filt.Field])
			if ok && !t.Before(from) && !t.After(to) {
				next[key] = row
			}
		}
		filtered = next
	}

	encoded, err := json.Marshal(remaining)
	if err != nil {
		return nil, nil, "encoding the remaining query params: " + err.Error()
	}
	return filtered, encoded, ""
}

// resolveDateRangeFilter resolves a dateRange filter's runtime queryParams
// value to concrete, inclusive [from, to] bounds in UTC.
//
// last7Days and lastCalendarMonth are computed relative to ref (nowFunc()
// at call time, a fixed instant in tests); custom uses the caller-supplied
// from/to literally. This is the documented (not schema-enforced) runtime
// convention Stage 1 recorded: {"kind": "last7Days"} or
// {"kind": "custom", "from": "2026-08-01", "to": "2026-08-31"}.
func resolveDateRangeFilter(preset string, val map[string]any, ref time.Time) (from, to time.Time, err error) {
	ref = ref.UTC()
	switch preset {
	case DateRangePresetLast7Days:
		to = startOfDay(ref)
		from = to.AddDate(0, 0, -6)
	case DateRangePresetLastCalendarMonth:
		firstOfThisMonth := time.Date(ref.Year(), ref.Month(), 1, 0, 0, 0, 0, time.UTC)
		lastMonthEnd := firstOfThisMonth.AddDate(0, 0, -1)
		from = time.Date(lastMonthEnd.Year(), lastMonthEnd.Month(), 1, 0, 0, 0, 0, time.UTC)
		to = startOfDay(lastMonthEnd)
	case DateRangePresetCustom:
		fromStr, _ := val["from"].(string)
		toStr, _ := val["to"].(string)
		if fromStr == "" || toStr == "" {
			return time.Time{}, time.Time{}, fmt.Errorf("a custom dateRange needs both \"from\" and \"to\"")
		}
		from, err = parseFilterDateString(fromStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("\"from\" %q: %w", fromStr, err)
		}
		to, err = parseFilterDateString(toStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("\"to\" %q: %w", toStr, err)
		}
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("unsupported dateRange preset %q", preset)
	}
	return from, to, nil
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// dateFilterLayouts are the formats a fixture's date field or a custom
// bound may use. "2006-01-02" is what every real scenario in this repo's
// own testdata uses; RFC3339 is accepted too since a real PocketBase "date"
// column round-trips through that format.
var dateFilterLayouts = []string{"2006-01-02", time.RFC3339, "2006-01-02 15:04:05.000Z"}

func parseFilterDateString(s string) (time.Time, error) {
	for _, layout := range dateFilterLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("not a recognized date (want YYYY-MM-DD or RFC3339)")
}

// parseFilterDate reads a row's field value as a date. A row whose value
// isn't a string, or doesn't parse, is excluded rather than guessed at —
// same "no vacuous pass" posture the rest of this verifier takes.
func parseFilterDate(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok || s == "" {
		return time.Time{}, false
	}
	t, err := parseFilterDateString(s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// fixtureStore builds a scratch event store holding the scenario's `given`.
func (v *verifier) fixtureStore(id, aggregate, stream string, given []EventRef) (*events.Store, error) {
	path := filepath.Join(v.dir, scaffold.SanitizeName(id)+".db")
	store, err := events.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening a scratch store: %w", err)
	}
	if len(given) == 0 {
		return store, nil
	}
	batch := make([]events.NewEvent, 0, len(given))
	for _, g := range given {
		data := g.Data
		if len(data) == 0 {
			data = json.RawMessage(`{}`)
		}
		batch = append(batch, events.NewEvent{Type: v.eventType(g.EventID), Data: data})
	}
	if _, err := store.Append(context.Background(), aggregate, stream, 0, batch); err != nil {
		store.Close()
		return nil, fmt.Errorf("seeding the scenario fixture: %w", err)
	}
	return store, nil
}

// splitGiven separates a scenario's history into the events this aggregate
// handles and those belonging elsewhere.
func (v *verifier) splitGiven(aggregate string, given []EventRef) (own, foreign []EventRef) {
	for _, g := range given {
		if owner, ok := v.eventAggregate[v.eventType(g.EventID)]; ok && owner != aggregate {
			foreign = append(foreign, g)
			continue
		}
		own = append(own, g)
	}
	return own, foreign
}

func (v *verifier) eventType(id string) string {
	ev, ok := v.doc.Events[id]
	if !ok {
		return TypeName("", id)
	}
	return TypeName(ev.Name, id)
}

func (v *verifier) commandName(id string) string {
	c, ok := v.doc.Commands[id]
	if !ok {
		return TypeName("", id)
	}
	return TypeName(c.Name, id)
}

// selectRows finds every row a query names.
//
// queryParams is a free-form object in the schema, so it is treated as a
// field match: every row whose fields equal every (filterable) parameter.
// With no params, every row is returned. By the time this runs, a scope
// param (filterByScopes) or a declared dateRange filter param
// (filterByFilters) has already been resolved and removed from params by
// the caller. A queryParam whose value is STILL a JSON object or array here
// names filtering logic no read model declared — there's no column and no
// Filters/Scopes entry it could match — so it's skipped rather than forced
// through a string/number comparison that could only ever fail; the ROW
// CONTENT comparison the caller does afterwards still reports the real gap
// the scenario is describing.
func selectRows(rows map[string]map[string]any, params json.RawMessage) []map[string]any {
	var want map[string]any
	if len(params) > 0 {
		_ = json.Unmarshal(params, &want)
	}
	for k, v := range want {
		switch v.(type) {
		case map[string]any, []any:
			delete(want, k)
		}
	}
	var found []map[string]any
	for _, row := range rows {
		ok := true
		for k, v := range want {
			if !jsonEq(row[scaffold.SanitizeName(k)], v) {
				ok = false
				break
			}
		}
		if ok {
			found = append(found, row)
		}
	}
	return found
}

// tryUnwrapExpectedList detects the named-list `then.result` shape: an
// object with exactly one property whose value is a JSON array. Every real
// scenario in the model uses this shape (a view is a list of rows); this
// package's own flat-object regression test (order-fulfillment.json) predates
// it and must keep working unchanged, which is why this is a narrow
// structural check rather than a required convention.
func tryUnwrapExpectedList(want json.RawMessage) ([]json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(want, &obj); err != nil || len(obj) != 1 {
		return nil, false
	}
	for _, raw := range obj {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, false
		}
		return arr, true
	}
	return nil, false
}

// listDiff is a multiset comparison for a named-list result: every expected
// row must subset-match a distinct actual row (order-independent), and no
// actual row may be left over — an extra row would mean an unscoped/scoped
// query returned something the scenario didn't ask for, which is as real a
// bug as a missing one.
func listDiff(expectedRows []json.RawMessage, actualRows []map[string]any) string {
	unmatched := append([]map[string]any(nil), actualRows...)
	var problems []string
	for _, want := range expectedRows {
		idx := -1
		for i, a := range unmatched {
			encoded, _ := json.Marshal(a)
			if subsetDiff(want, encoded) == "" {
				idx = i
				break
			}
		}
		if idx < 0 {
			problems = append(problems, fmt.Sprintf("no row matched %s", string(want)))
			continue
		}
		unmatched = append(unmatched[:idx], unmatched[idx+1:]...)
	}
	if len(unmatched) > 0 {
		problems = append(problems, fmt.Sprintf("%d extra row(s) not expected by any element of the result list", len(unmatched)))
	}
	return strings.Join(problems, "; ")
}

// subsetDiff reports how `actual` fails to contain every field of `want`,
// or "" when it does.
//
// A subset comparison, not equality: a scenario states the fields it cares
// about, and the generated code legitimately carries more (an aggregate id
// the document never mentions, say). Requiring equality would fail every
// scenario for a reason that is not about the domain.
func subsetDiff(want, actual json.RawMessage) string {
	var w, a map[string]any
	if err := json.Unmarshal(want, &w); err != nil {
		return "expected value is not an object"
	}
	if err := json.Unmarshal(actual, &a); err != nil {
		return "produced value is not an object"
	}
	keys := make([]string, 0, len(w))
	for k := range w {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var diffs []string
	for _, k := range keys {
		got, present := a[scaffold.SanitizeName(k)]
		if !present {
			diffs = append(diffs, fmt.Sprintf("%s missing", k))
			continue
		}
		if !jsonEq(got, w[k]) {
			diffs = append(diffs, fmt.Sprintf("%s = %v, expected %v", k, got, w[k]))
		}
	}
	return strings.Join(diffs, "; ")
}

func jsonEq(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}
