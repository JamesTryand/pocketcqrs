package emschema

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
	"github.com/jamestryand/pocketcqrs/scaffold"
)

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
	viewStream := v.streamID(sc.Given)
	fixture := make([]events.Event, 0, len(sc.Given))
	for i, g := range sc.Given {
		data := g.Data
		if len(data) == 0 {
			data = json.RawMessage(`{}`)
		}
		fixture = append(fixture, events.Event{
			Position: int64(i + 1), ID: fmt.Sprintf("scenario-%d", i+1),
			Aggregate: v.doc.Events[g.EventID].Aggregate, AggregateID: viewStream,
			Sequence: int64(i + 1), Type: v.eventType(g.EventID), Data: data,
			Created: "1970-01-01 00:00:00.000Z",
		})
	}
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
	scopedRows, remainingParams, scopeErr := v.filterByScopes(rm, rows, q.QueryParams, fixture)
	if scopeErr != "" {
		res.Detail = scopeErr
		return
	}
	match := selectRow(scopedRows, remainingParams)
	if match == nil {
		res.Detail = fmt.Sprintf("no row matched the query; the projection produced %d row(s)", len(rows))
		return
	}
	encoded, _ := json.Marshal(match)
	if diff := subsetDiff(then.Result, encoded); diff != "" {
		res.Detail = "result: " + diff
		return
	}
	res.Passed = true
	res.Detail = "the projected row matches"
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
// The via read model is folded over the SAME fixture as the target: a
// stateView scenario already collapses every given event onto one synthetic
// stream (see streamID/viewStream above), so the via projection's own row
// for that stream carries whatever `matchParamTo`/`selectField` values the
// fixture's events gave it. This mirrors the real semi-join
// (`WHERE filterLocalField IN (SELECT selectField FROM via WHERE
// matchParamTo = :param)`) at verify-scenario grain: one candidate via row,
// checked against one candidate target row set.
func (v *verifier) filterByScopes(rm ReadModel, rows map[string]map[string]any, queryParams json.RawMessage, fixture []events.Event) (map[string]map[string]any, json.RawMessage, string) {
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
		run, err := functions.DryRunProjectionOver(spec, fixture)
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

// selectRow finds the row a query names.
//
// queryParams is a free-form object in the schema, so it is treated as a
// field match: the row whose fields equal every parameter. With no params,
// the only row is returned — and an ambiguous query returns nothing rather
// than an arbitrary row, because picking one would make the assertion depend
// on map iteration order.
func selectRow(rows map[string]map[string]any, params json.RawMessage) map[string]any {
	var want map[string]any
	if len(params) > 0 {
		_ = json.Unmarshal(params, &want)
	}
	var found map[string]any
	matches := 0
	for _, row := range rows {
		ok := true
		for k, v := range want {
			if !jsonEq(row[scaffold.SanitizeName(k)], v) {
				ok = false
				break
			}
		}
		if ok {
			matches++
			found = row
		}
	}
	if matches != 1 {
		return nil
	}
	return found
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
