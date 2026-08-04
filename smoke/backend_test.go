//go:build smoke

package smoke

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// poisonFn fails every delivery, so the dead-letter queue has something real
// in it. A healthy system exercises none of the interesting operational UI.
const poisonFn = `//@trigger event TaskCreated
throw new Error("poison: sink refused " + event.type + " @" + event.position);
`

const fixedFn = `//@trigger event TaskCreated
console.log("sink accepted " + event.type);
`

// TestOperationalAPI covers the read side of the operational API against a
// real log: the feed and its bounds, streams, the catalog's head-of-log, and
// the mode barrier.
func TestOperationalAPI(t *testing.T) {
	h := startBackend(t, nil)
	h.command("task", "t1", "CreateTask", map[string]string{"title": "first"})
	h.command("task", "t2", "CreateTask", map[string]string{"title": "second"})
	h.command("task", "t1", "CompleteTask", map[string]any{})

	var feed struct {
		Events []struct {
			Position    int64  `json:"position"`
			Aggregate   string `json:"aggregate"`
			AggregateID string `json:"aggregateId"`
			Type        string `json:"type"`
		} `json:"events"`
	}
	h.apiOK(http.MethodGet, "/api/cqrs/events", nil, &feed)
	if len(feed.Events) != 3 {
		t.Fatalf("expected 3 events in the feed, got %d", len(feed.Events))
	}
	for i := 1; i < len(feed.Events); i++ {
		if feed.Events[i].Position <= feed.Events[i-1].Position {
			t.Fatal("the feed must come back in ascending position order")
		}
	}

	// aggregateId narrows to one stream; `before` pages backwards, which is
	// not the same as after−limit under a filter
	var stream struct{ Events []struct{ Position int64 } }
	h.apiOK(http.MethodGet, "/api/cqrs/events?aggregate=task&aggregateId=t1", nil, &stream)
	if len(stream.Events) != 2 {
		t.Fatalf("expected t1 to have 2 events, got %d", len(stream.Events))
	}
	head := feed.Events[len(feed.Events)-1].Position
	var older struct{ Events []struct{ Position int64 } }
	h.apiOK(http.MethodGet, "/api/cqrs/events?before="+strconv.FormatInt(head, 10), nil, &older)
	for _, e := range older.Events {
		if e.Position >= head {
			t.Fatalf("before=%d returned position %d", head, e.Position)
		}
	}

	var streams struct {
		Streams []struct {
			Aggregate    string `json:"aggregate"`
			AggregateID  string `json:"aggregateId"`
			Events       int64  `json:"events"`
			LastPosition int64  `json:"lastPosition"`
		} `json:"streams"`
	}
	h.apiOK(http.MethodGet, "/api/cqrs/streams?aggregate=task", nil, &streams)
	if len(streams.Streams) != 2 {
		t.Fatalf("expected 2 task streams, got %d", len(streams.Streams))
	}

	// behind-by is measured against maxPosition, never the event count:
	// position is AUTOINCREMENT and conflict retries burn values
	var catalog struct {
		Mode   string `json:"mode"`
		Totals struct {
			Events      int64 `json:"events"`
			MaxPosition int64 `json:"maxPosition"`
		} `json:"totals"`
		Consumers []struct {
			Name       string `json:"name"`
			Checkpoint int64  `json:"checkpoint"`
		} `json:"consumers"`
	}
	h.apiOK(http.MethodGet, "/api/cqrs/catalog", nil, &catalog)
	if catalog.Totals.MaxPosition != head {
		t.Fatalf("catalog head-of-log %d disagrees with the feed's last position %d",
			catalog.Totals.MaxPosition, head)
	}
	if catalog.Mode != "running" {
		t.Fatalf("expected to start running, got %q", catalog.Mode)
	}

	// the barrier: commands are refused in maintenance, and the mode is
	// validated so a bad value cannot leave the system in limbo
	h.setMode("maintenance")
	status, _ := h.api(http.MethodPost, "/api/cqrs/task/t3/CreateTask", jsonBody(map[string]string{"title": "x"}), nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for a command in maintenance, got %d", status)
	}
	status, _ = h.api(http.MethodPost, "/api/cqrs/admin/mode", jsonBody(map[string]string{"mode": "bogus"}), nil)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid mode, got %d", status)
	}
	var mode struct {
		Mode string `json:"mode"`
	}
	h.apiOK(http.MethodGet, "/api/cqrs/admin/mode", nil, &mode)
	if mode.Mode != "maintenance" {
		t.Fatalf("a refused mode change moved the system to %q", mode.Mode)
	}
	h.setMode("running")

	// every operational route is superuser-scoped
	unauth := newClient(t)
	for _, path := range []string{"/api/cqrs/events", "/api/cqrs/streams", "/api/cqrs/deadletters",
		"/api/cqrs/catalog", "/api/cqrs/admin/mode", "/api/cqrs/admin/functions"} {
		resp, err := unauth.Get(h.BackendURL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a token: expected 401, got %d", path, resp.StatusCode)
		}
	}
}

// TestDeadLetterLifecycle drives the whole operator story: a poison function
// dead-letters real events, a retry that still fails is a RESULT not an
// error, dismissal resolves without re-delivering, and fixing the function
// then retrying resolves the rest.
func TestDeadLetterLifecycle(t *testing.T) {
	h := startBackend(t, map[string]string{"poison.js": poisonFn})
	for _, id := range []string{"t1", "t2", "t3"} {
		h.command("task", id, "CreateTask", map[string]string{"title": id})
	}

	type deadLetter struct {
		ID       int64  `json:"id"`
		Consumer string `json:"consumer"`
		Attempts int64  `json:"attempts"`
		Resolved bool   `json:"resolved"`
		Error    string `json:"error"`
	}
	letters := func(all bool) []deadLetter {
		var out struct {
			DeadLetters []deadLetter `json:"deadLetters"`
		}
		path := "/api/cqrs/deadletters"
		if all {
			path += "?all=1"
		}
		h.apiOK(http.MethodGet, path, nil, &out)
		return out.DeadLetters
	}
	eventually(t, "three poisoned deliveries to be dead-lettered", func() bool {
		return len(letters(false)) == 3
	})

	pending := letters(false)
	// the routes are more specific than the gateway's
	// POST /api/cqrs/{aggregate}/{id}/{command}, so ServeMux must prefer
	// them — if precedence ever flips, this answers "unknown aggregate"
	var retry struct {
		ID       int64  `json:"id"`
		Resolved bool   `json:"resolved"`
		Attempts int64  `json:"attempts"`
		Error    string `json:"error"`
	}
	h.apiOK(http.MethodPost, "/api/cqrs/deadletters/"+strconv.FormatInt(pending[0].ID, 10)+"/retry", nil, &retry)
	if retry.Resolved || retry.Error == "" {
		t.Fatalf("a still-poison retry should report resolved=false with the failure: %+v", retry)
	}
	if retry.Attempts != pending[0].Attempts+1 {
		t.Fatalf("a failed retry must record an attempt: was %d, now %d", pending[0].Attempts, retry.Attempts)
	}

	// dismissal resolves without re-delivering
	h.apiOK(http.MethodPost, "/api/cqrs/deadletters/"+strconv.FormatInt(pending[1].ID, 10)+"/dismiss", nil, nil)
	if got := len(letters(false)); got != 2 {
		t.Fatalf("expected 2 pending after a dismissal, got %d", got)
	}

	// 4xx is reserved for a bad id, never for a delivery that failed again
	if status, _ := h.api(http.MethodPost, "/api/cqrs/deadletters/99999/retry", nil, nil); status != http.StatusNotFound {
		t.Errorf("unknown dead-letter id: expected 404, got %d", status)
	}
	if status, _ := h.api(http.MethodPost, "/api/cqrs/deadletters/abc/retry", nil, nil); status != http.StatusBadRequest {
		t.Errorf("malformed dead-letter id: expected 400, got %d", status)
	}

	// fix the function, reload it, retry the batch: retries run through the
	// CURRENT code, which is the entire point of the workflow
	h.apiOK(http.MethodPut, "/api/cqrs/admin/functions/poison.js",
		jsonBody(map[string]string{"source": fixedFn}), nil)
	h.reload()
	var all struct {
		Results []struct {
			Resolved bool `json:"resolved"`
		} `json:"results"`
	}
	h.apiOK(http.MethodPost, "/api/cqrs/deadletters/retry", nil, &all)
	if len(all.Results) != 2 {
		t.Fatalf("expected 2 letters retried, got %d", len(all.Results))
	}
	for _, r := range all.Results {
		if !r.Resolved {
			t.Error("a retry through the fixed code should have resolved")
		}
	}
	if got := len(letters(false)); got != 0 {
		t.Fatalf("expected nothing pending after the batch retry, got %d", got)
	}
	if got := len(letters(true)); got != 3 {
		t.Fatalf("resolved letters should still be listed with ?all=1, got %d", got)
	}
}

// TestFunctionFileAPI is the security-and-safety test for the routes that
// write code the server executes.
func TestFunctionFileAPI(t *testing.T) {
	h := startBackend(t, map[string]string{"audit.js": fixedFn})

	var list struct {
		Dir   string `json:"dir"`
		Files []struct {
			Name        string `json:"name"`
			Declaration *struct {
				Kind          string `json:"kind"`
				SchemaBearing bool   `json:"schemaBearing"`
			} `json:"declaration"`
			Error string `json:"error"`
		} `json:"files"`
	}
	h.apiOK(http.MethodGet, "/api/cqrs/admin/functions", nil, &list)
	if len(list.Files) != 1 || list.Files[0].Name != "audit.js" {
		t.Fatalf("unexpected listing: %+v", list.Files)
	}
	if list.Files[0].Declaration == nil || list.Files[0].Declaration.Kind != "effect" {
		t.Fatalf("the listing must classify the file as the loader does: %+v", list.Files[0])
	}

	// Path traversal, through the real router with real percent-encoding.
	// The name check is the whole control surface here.
	for _, bad := range []string{
		"..%2Fescape.js", "..%5Cescape.js", "sub%2Fnested.js",
		"CON.js", "nul.js", "plain.txt", ".hidden.js",
	} {
		status, _ := h.api(http.MethodPut, "/api/cqrs/admin/functions/"+bad,
			jsonBody(map[string]string{"source": fixedFn}), nil)
		if status == http.StatusOK {
			t.Errorf("%s was accepted; it must be refused", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(h.FunctionsDir, "..", "escape.js")); err == nil {
		t.Fatal("a write escaped the functions directory")
	}

	// A write is refused unless the source LOADS: reloads are
	// all-or-nothing, so an unloadable file would abort every later reload,
	// including the one that fixes it.
	for name, src := range map[string]string{
		"broken.js":  "//@trigger event TaskCreated\nthis is not javascript (((\n",
		"badcron.js": "//@trigger cron 99 99 99 99 99\nconsole.log('tick');\n",
		"badproj.js": "//@trigger projection p on TaskCreated\n//@schema\n//@key a\nfunction project() {}\n",
	} {
		status, raw := h.api(http.MethodPut, "/api/cqrs/admin/functions/"+name,
			jsonBody(map[string]string{"source": src}), nil)
		if status != http.StatusBadRequest {
			t.Errorf("%s: expected the write to be refused, got %d: %s", name, status, truncate(raw, 200))
		}
		if _, err := os.Stat(filepath.Join(h.FunctionsDir, name)); err == nil {
			t.Errorf("%s: a refused write left the file on disk", name)
		}
	}

	// and the refusal did not break the system: a reload still works
	if report := h.reload(); report["schemaTier"] == nil {
		t.Fatal("reload did not report a schema tier after refused writes")
	}

	// a valid schema-bearing file saves, is reported inert, and needs the
	// barrier to activate
	const projection = `//@trigger projection titles on TaskCreated
//@schema titles title:text
//@key title
function project(event) { return [{ upsert: { key: event.data.title, fields: { title: event.data.title } } }]; }
`
	var save struct {
		Name        string `json:"name"`
		Active      bool   `json:"active"`
		Hint        string `json:"hint"`
		Declaration struct {
			Kind          string `json:"kind"`
			SchemaBearing bool   `json:"schemaBearing"`
		} `json:"declaration"`
	}
	h.apiOK(http.MethodPut, "/api/cqrs/admin/functions/titles.js",
		jsonBody(map[string]string{"source": projection}), &save)
	if save.Active || !save.Declaration.SchemaBearing || !strings.Contains(save.Hint, "maintenance") {
		t.Fatalf("saving must report inert + schema-bearing + the barrier: %+v", save)
	}

	// read it back
	var read struct {
		Source      string `json:"source"`
		HasPrevious bool   `json:"hasPrevious"`
	}
	h.apiOK(http.MethodGet, "/api/cqrs/admin/functions/titles.js", nil, &read)
	if !strings.Contains(read.Source, "//@trigger projection titles") {
		t.Fatal("reading a file did not return the source that was written")
	}
	if read.HasPrevious {
		t.Error("a file written once should have no previous version")
	}
	if status, _ := h.api(http.MethodGet, "/api/cqrs/admin/functions/titles.js/previous", nil, nil); status != http.StatusNotFound {
		t.Errorf("expected 404 for a file that was never overwritten, got %d", status)
	}

	// overwriting keeps the replaced copy: without it a mis-paste through
	// the editor is unrecoverable on a deployment whose functions directory
	// is not version-controlled
	const replacement = `//@trigger projection titles on TaskCreated
//@schema titles title:text
//@key title
function project(event) { return []; }
`
	h.apiOK(http.MethodPut, "/api/cqrs/admin/functions/titles.js",
		jsonBody(map[string]string{"source": replacement}), nil)
	h.apiOK(http.MethodGet, "/api/cqrs/admin/functions/titles.js", nil, &read)
	if !read.HasPrevious || !strings.Contains(read.Source, "return []") {
		t.Fatalf("the overwrite did not take, or no previous version was kept: %+v", read)
	}
	var prev struct {
		Source string `json:"source"`
	}
	h.apiOK(http.MethodGet, "/api/cqrs/admin/functions/titles.js/previous", nil, &prev)
	if !strings.Contains(prev.Source, "upsert") {
		t.Fatalf("the previous version is not the source that was replaced: %q", truncate(prev.Source, 120))
	}
	// the kept copy is not a .js file, so it must not appear as one
	var afterList struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	h.apiOK(http.MethodGet, "/api/cqrs/admin/functions", nil, &afterList)
	for _, f := range afterList.Files {
		if strings.HasSuffix(f.Name, ".prev") {
			t.Errorf("the kept copy %q is listed as a function file", f.Name)
		}
	}

	// delete
	h.apiOK(http.MethodDelete, "/api/cqrs/admin/functions/titles.js", nil, nil)
	if status, _ := h.api(http.MethodGet, "/api/cqrs/admin/functions/titles.js", nil, nil); status != http.StatusNotFound {
		t.Errorf("expected 404 after deleting, got %d", status)
	}
}

// TestDryRunAPI: the dry run answers "would a reload accept this, and what
// would it do" without persisting anything.
func TestDryRunAPI(t *testing.T) {
	h := startBackend(t, nil)
	h.command("task", "t1", "CreateTask", map[string]string{"title": "one"})
	h.command("task", "t2", "CreateTask", map[string]string{"title": "two"})
	// a SECOND event type in the history, so that a decider declaring only
	// //@handles TaskCreated is genuinely incomplete — without this the
	// coverage check below passes vacuously and tests nothing
	h.command("task", "t1", "CompleteTask", map[string]any{})

	dry := func(req map[string]any) (int, map[string]any) {
		var out map[string]any
		status, raw := h.api(http.MethodPost, "/api/cqrs/admin/dryrun", jsonBody(req), &out)
		if status != http.StatusOK && out == nil {
			t.Fatalf("dryrun: %d: %s", status, truncate(raw, 200))
		}
		return status, out
	}

	// a projection returning plain objects instead of row ops folds events
	// and produces nothing — the mistake the panel exists to surface
	const wrong = `//@trigger projection titles on TaskCreated
//@schema titles title:text
//@key title
function project(event) { return [{ title: event.data.title }]; }
`
	status, out := dry(map[string]any{"name": "titles.js", "source": wrong, "mode": "projection"})
	if status != http.StatusOK {
		t.Fatalf("a wrong-contract projection should still RUN, got %d", status)
	}
	if out["events"].(float64) < 2 || out["upserts"].(float64) != 0 {
		t.Fatalf("expected folded events with zero upserts, got %v", out)
	}
	// and it must NAME the mistake rather than leave a zero to be
	// interpreted: the same values are silently discarded at runtime
	if out["ignoredValues"] == nil || out["ignoredValues"].(float64) < 2 {
		t.Fatalf("the dry run should report the values that are not row ops: %v", out)
	}
	if !strings.Contains(out["summary"].(string), "NOT row ops") {
		t.Errorf("the summary should say so in prose: %q", out["summary"])
	}

	const right = `//@trigger projection titles on TaskCreated
//@schema titles title:text
//@key title
function project(event) { return [{ upsert: { key: event.data.title, fields: { title: event.data.title } } }]; }
`
	_, out = dry(map[string]any{"name": "titles.js", "source": right, "mode": "projection"})
	if out["upserts"].(float64) < 2 {
		t.Fatalf("the corrected projection should upsert: %v", out)
	}

	// mode=decider applies the gate a RELOAD applies, not just a fold:
	// folding alone passes vacuously for an aggregate with no history
	for name, src := range map[string]string{
		"missing decide()": "//@trigger decider probe\n//@handles ProbeCreated\nfunction initialState(){return {};}\nfunction evolve(s,e){return s;}\n",
		"evolve throws on real history": "//@trigger decider task\n//@handles TaskCreated TaskCompleted\n" +
			"function initialState(){return {};}\nfunction decide(){return [];}\nfunction evolve(){throw new Error('boom');}\n",
		"incomplete //@handles": "//@trigger decider task\n//@handles TaskCreated\n" +
			"function initialState(){return {};}\nfunction decide(){return [];}\nfunction evolve(s,e){return s;}\n",
	} {
		if status, _ := dry(map[string]any{"name": "d.js", "source": src, "mode": "decider"}); status != http.StatusBadRequest {
			t.Errorf("%s: expected the dry run to refuse it, got %d", name, status)
		}
	}

	// decide reports what a command WOULD append, and appends nothing
	const decider = `//@trigger decider probe
//@handles ProbeCreated
function initialState() { return { made: false }; }
function decide(command, state) {
  if (command.name === 'MakeProbe') { return [{ type: 'ProbeCreated', data: { by: command.payload.by } }]; }
  return [];
}
function evolve(state, event) { return { made: true }; }
`
	_, out = dry(map[string]any{"name": "probe.js", "source": decider, "mode": "decide",
		"streamId": "p1", "command": "MakeProbe", "payload": map[string]string{"by": "smoke"}})
	produced, ok := out["produced"].([]any)
	if !ok || len(produced) != 1 {
		t.Fatalf("decide should report one produced event: %v", out)
	}
	var probeFeed struct{ Events []struct{ Type string } }
	h.apiOK(http.MethodGet, "/api/cqrs/events?aggregate=probe", nil, &probeFeed)
	if len(probeFeed.Events) != 0 {
		t.Fatal("a dry run appended events to the real log")
	}

	// an unknown mode is refused rather than guessed
	if status, _ := dry(map[string]any{"name": "x.js", "source": fixedFn, "mode": "guess"}); status != http.StatusBadRequest {
		t.Errorf("an unknown dryrun mode should be refused, got %d", status)
	}
}

// TestScaffoldedSliceWorks is the DASH.6 payoff: a slice described in a few
// fields is generated, saved, activated, and then actually accepts commands
// and materialises a read model. Generating source that looks right but does
// not RUN would be worse than not generating at all.
func TestScaffoldedSliceWorks(t *testing.T) {
	h := startBackend(t, nil)

	var gen struct {
		Files []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
			Kind   string `json:"kind"`
		} `json:"files"`
	}
	h.apiOK(http.MethodPost, "/api/cqrs/admin/scaffold", jsonBody(map[string]any{
		"aggregate": "ticket",
		"commands": []map[string]any{
			{"name": "OpenTicket", "event": "TicketOpened", "once": true,
				"fields": []map[string]string{{"name": "subject", "type": "text"}}},
			{"name": "CloseTicket", "event": "TicketClosed", "requiresExisting": true,
				"fields": []map[string]string{{"name": "resolution", "type": "text"}}},
		},
		"readModel": map[string]any{
			"collection": "tickets", "key": "ticketId",
			"fields": []map[string]string{
				{"name": "subject", "type": "text"}, {"name": "resolution", "type": "text"},
			},
		},
	}), &gen)
	if len(gen.Files) != 2 {
		t.Fatalf("expected a decider and a projection, got %d", len(gen.Files))
	}

	// generation writes nothing — the files go through the ordinary save
	// path, load check included
	for _, f := range gen.Files {
		if status, _ := h.api(http.MethodGet, "/api/cqrs/admin/functions/"+f.Name, nil, nil); status != http.StatusNotFound {
			t.Fatalf("%s exists before it was saved: generation must not write", f.Name)
		}
		// and each passes its own dry run before anyone saves it
		var dry map[string]any
		status, raw := h.api(http.MethodPost, "/api/cqrs/admin/dryrun",
			jsonBody(map[string]any{"name": f.Name, "source": f.Source, "mode": f.Kind}), &dry)
		if status != http.StatusOK {
			t.Fatalf("generated %s failed its own dry run: %s", f.Name, truncate(raw, 300))
		}
		h.apiOK(http.MethodPut, "/api/cqrs/admin/functions/"+f.Name,
			jsonBody(map[string]string{"source": f.Source}), nil)
	}

	// activate the slice behind the barrier
	h.setMode("maintenance")
	report := h.reload()
	h.setMode("running")
	if report["schemaTier"] != "reloaded" {
		t.Fatalf("the slice did not activate: %v", report)
	}

	// and now it is a working vertical slice
	h.command("ticket", "tk1", "OpenTicket", map[string]string{"subject": "printer on fire"})
	h.command("ticket", "tk1", "CloseTicket", map[string]string{"resolution": "extinguished"})

	// the generated decider's rules hold: the create is once-only, and the
	// follow-up needs an existing stream
	if status, _ := h.api(http.MethodPost, "/api/cqrs/ticket/tk1/OpenTicket",
		jsonBody(map[string]string{"subject": "again"}), nil); status != http.StatusBadRequest {
		t.Errorf("re-opening an existing ticket should be refused, got %d", status)
	}
	if status, _ := h.api(http.MethodPost, "/api/cqrs/ticket/unknown/CloseTicket",
		jsonBody(map[string]string{"resolution": "x"}), nil); status != http.StatusBadRequest {
		t.Errorf("closing a ticket that does not exist should be refused, got %d", status)
	}

	// the generated projection maintains the read model — the row-op
	// contract is the thing most easily got wrong, so assert real rows
	var rows struct {
		Items []struct {
			TicketID   string `json:"ticketId"`
			Subject    string `json:"subject"`
			Resolution string `json:"resolution"`
		} `json:"items"`
	}
	eventually(t, "the generated projection to materialise its row", func() bool {
		status, _ := h.api(http.MethodGet, "/api/collections/tickets/records", nil, &rows)
		return status == http.StatusOK && len(rows.Items) == 1
	})
	got := rows.Items[0]
	if got.TicketID != "tk1" || got.Subject != "printer on fire" || got.Resolution != "extinguished" {
		t.Fatalf("the projection merged the events wrongly: %+v", got)
	}

	// the declared commands reach the catalog, which is what makes an
	// export faithful — commands leave no trace in the log to recover
	var cat struct {
		Aggregates []struct {
			Name     string   `json:"name"`
			Commands []string `json:"commands"`
		} `json:"aggregates"`
	}
	h.apiOK(http.MethodGet, "/api/cqrs/catalog", nil, &cat)
	found := false
	for _, a := range cat.Aggregates {
		if a.Name == "ticket" {
			found = true
			if len(a.Commands) != 2 {
				t.Errorf("expected the declared commands in the catalog, got %v", a.Commands)
			}
		}
	}
	if !found {
		t.Error("the scaffolded aggregate is not in the catalog")
	}
}

// TestAuthoringWorkflow is the DASH.5 payoff end to end: write a projection
// through the API, watch it stay inert, activate it behind the barrier, and
// see it live with history replayed and its collection write-guarded.
func TestAuthoringWorkflow(t *testing.T) {
	h := startBackend(t, nil)
	h.command("task", "t1", "CreateTask", map[string]string{"title": "alpha"})
	h.command("task", "t2", "CreateTask", map[string]string{"title": "beta"})

	const projection = `//@trigger projection smoke_titles on TaskCreated
//@schema smoke_titles title:text
//@key title
function project(event) { return [{ upsert: { key: event.data.title, fields: { title: event.data.title } } }]; }
`
	h.apiOK(http.MethodPut, "/api/cqrs/admin/functions/smoke_titles.js",
		jsonBody(map[string]string{"source": projection}), nil)

	// saved is not activated: the collection does not exist yet
	if status, _ := h.api(http.MethodGet, "/api/collections/smoke_titles/records", nil, nil); status != http.StatusNotFound {
		t.Fatalf("a saved file should be inert until reload, got %d", status)
	}
	// nor does reloading while RUNNING move the schema tier
	if report := h.reload(); !strings.HasPrefix(report["schemaTier"].(string), "skipped") {
		t.Fatalf("the schema tier must not move while running: %v", report["schemaTier"])
	}

	h.setMode("maintenance")
	report := h.reload()
	if report["schemaTier"] != "reloaded" {
		t.Fatalf("expected the schema tier to reload behind the barrier: %v", report)
	}
	h.setMode("running")

	var records struct {
		Items []struct {
			Title string `json:"title"`
		} `json:"items"`
	}
	eventually(t, "the projection to replay history into its collection", func() bool {
		status, _ := h.api(http.MethodGet, "/api/collections/smoke_titles/records", nil, &records)
		return status == http.StatusOK && len(records.Items) >= 2
	})
	titles := map[string]bool{}
	for _, r := range records.Items {
		titles[r.Title] = true
	}
	if !titles["alpha"] || !titles["beta"] {
		t.Fatalf("the projection did not replay the log: %+v", records.Items)
	}

	// a newly declared collection is write-guarded like any other
	status, _ := h.api(http.MethodPost, "/api/collections/smoke_titles/records",
		jsonBody(map[string]string{"title": "direct"}), nil)
	if status != http.StatusForbidden {
		t.Fatalf("direct writes to a projection-owned collection must be denied, got %d", status)
	}
}
