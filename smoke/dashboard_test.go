//go:build smoke

package smoke

import (
	"net/http"
	"strings"
	"testing"
)

// TestDashboardBrowsing walks every read-only page against a real backend.
// The dashboard's own unit tests use a fake backend, so this is the only
// check that the two agree about the API contract.
func TestDashboardBrowsing(t *testing.T) {
	h := startBackend(t, map[string]string{"audit.js": fixedFn})
	h.startDashboard()
	h.command("task", "t1", "CreateTask", map[string]string{"title": "alpha"})
	h.command("task", "t1", "CompleteTask", map[string]any{})

	// unauthenticated, every page bounces to the sign-in screen
	for _, path := range []string{"/", "/aggregates", "/events", "/consumers", "/system", "/functions"} {
		status, _ := h.dash(http.MethodGet, path, nil, false)
		if status != http.StatusSeeOther {
			t.Errorf("%s without a session: expected 303, got %d", path, status)
		}
	}

	h.login()

	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/", []string{"Overview", "Events in log", `id="explorer"`, `id="catalog-data"`, "flowchart"}},
		{"/aggregates", []string{"task", `href="/aggregates/task"`, "TaskCreated"}},
		{"/aggregates/task", []string{`href="/aggregates/task/t1"`}},
		{"/aggregates/task/t1", []string{"postit postit-event", "TaskCreated", "TaskCompleted"}},
		{"/events", []string{"TaskCreated", "All aggregates"}},
		{"/consumers", []string{"Checkpoints", "Dead letters", "caught up"}},
		{"/system", []string{"Mode barrier", "Hot reload", "Running."}},
		{"/functions", []string{"audit.js", "pb_functions", `id="function-source"`}},
	} {
		status, body := h.dash(http.MethodGet, tc.path, nil, false)
		if status != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", tc.path, status)
			continue
		}
		if err := contains(body, tc.want...); err != nil {
			t.Errorf("%s: %v", tc.path, err)
		}
	}
}

// TestDashboardLiveFragments checks the polling contract end to end: the
// page carries a trigger, the fragment is a fragment, and it renders its own
// trigger — which is what keeps the loop alive, since htmx 4 has no
// stop-polling status code.
func TestDashboardLiveFragments(t *testing.T) {
	h := startBackend(t, nil)
	h.startDashboard()
	h.login()

	for _, tc := range []struct{ page, fragment, marker string }{
		{"/", "/fragments/overview", `id="overview-live"`},
		{"/consumers", "/fragments/checkpoints", `id="checkpoints-body"`},
	} {
		_, page := h.dash(http.MethodGet, tc.page, nil, false)
		if err := contains(page, `hx-get="`+tc.fragment+`"`, `hx-trigger="every 2s"`); err != nil {
			t.Errorf("%s does not poll: %v", tc.page, err)
		}

		status, frag := h.dash(http.MethodGet, tc.fragment, nil, true)
		if status != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", tc.fragment, status)
			continue
		}
		if strings.Contains(strings.ToLower(frag), "<!doctype") {
			t.Errorf("%s returned a whole page", tc.fragment)
		}
		if err := contains(frag, tc.marker, `hx-trigger="every 2s"`); err != nil {
			t.Errorf("%s: %v — polling would stop after one tick", tc.fragment, err)
		}
	}

	// the log browser deliberately does not poll: an unbounded page is the
	// START of the log, so a timer would refetch the same rows forever
	if _, body := h.dash(http.MethodGet, "/events", nil, false); strings.Contains(body, "hx-trigger") {
		t.Error("the log browser should not poll")
	}
}

// TestDashboardExpiredSessionUnderPoll is the defect the fragment design
// exists to prevent: a 303 would be followed by htmx's fetch and the whole
// sign-in page swapped into a table body, every two seconds, forever.
func TestDashboardExpiredSessionUnderPoll(t *testing.T) {
	h := startBackend(t, nil)
	h.startDashboard()
	h.login()

	// the session is valid, but the token the cookie carries is not: this is
	// what an expired or revoked superuser token looks like to the dashboard
	h.client.Jar.SetCookies(mustURL(t, h.DashboardURL), []*http.Cookie{{
		Name: "pcqrs_auth", Value: "stale-token", Path: "/",
	}})

	for _, path := range []string{"/fragments/checkpoints", "/fragments/overview", "/fragments/deadletters"} {
		resp, body := h.dashHeaders(http.MethodGet, path, nil, true)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: expected 401 for htmx, got %d", path, resp.StatusCode)
		}
		if resp.Header.Get("HX-Redirect") != "/login" {
			t.Errorf("%s: expected HX-Redirect to /login, got %q", path, resp.Header.Get("HX-Redirect"))
		}
		if strings.Contains(body, "<form") {
			t.Errorf("%s: a sign-in page was returned to a polling element", path)
		}
	}

	// a plain navigation still gets the ordinary redirect
	resp, _ := h.dashHeaders(http.MethodGet, "/fragments/checkpoints", nil, false)
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("a non-htmx request should get a 303, got %d", resp.StatusCode)
	}
}

// TestDashboardActions drives the write side of the dashboard: the mode
// barrier, hot reload with its report, and dead-letter retry/dismiss.
func TestDashboardActions(t *testing.T) {
	h := startBackend(t, map[string]string{"poison.js": poisonFn})
	h.startDashboard()
	h.login()
	h.command("task", "t1", "CreateTask", map[string]string{"title": "one"})

	// --- mode toggle: POST-then-redirect, so a refresh cannot re-fire it
	resp, _ := h.dashHeaders(http.MethodPost, "/system/mode",
		formBody(map[string]string{"mode": "maintenance"}), false)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/system" {
		t.Fatalf("mode toggle: expected 303 to /system, got %d %q",
			resp.StatusCode, resp.Header.Get("Location"))
	}
	var mode struct {
		Mode string `json:"mode"`
	}
	h.apiOK(http.MethodGet, "/api/cqrs/admin/mode", nil, &mode)
	if mode.Mode != "maintenance" {
		t.Fatalf("the backend did not move to maintenance: %q", mode.Mode)
	}

	// a value the backend refuses is reported as a refused TOGGLE, naming
	// the value — not as a generic backend error an operator could misread
	status, body := h.dash(http.MethodPost, "/system/mode", formBody(map[string]string{"mode": "bogus"}), false)
	if status != http.StatusBadRequest {
		t.Errorf("a refused mode: expected 400, got %d", status)
	}
	if err := contains(body, "The mode was not changed", "bogus"); err != nil {
		t.Errorf("refused mode: %v", err)
	}

	// --- reload: the report is RENDERED, never redirected away
	resp, body = h.dashHeaders(http.MethodPost, "/system/reload", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reload: expected 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Vary") != "HX-Request" {
		t.Errorf("branching on HX-Request without Vary: %q", resp.Header.Get("Vary"))
	}
	if strings.Contains(strings.ToLower(body), "<!doctype") {
		t.Error("an htmx reload returned a whole page")
	}
	if err := contains(body, `id="reload-report"`, "poison.js"); err != nil {
		t.Errorf("reload report: %v", err)
	}
	h.dash(http.MethodPost, "/system/mode", formBody(map[string]string{"mode": "running"}), false)

	// --- dead letters: a retry that fails again is a RESULT, not an error
	var pending struct {
		DeadLetters []struct {
			ID int64 `json:"id"`
		} `json:"deadLetters"`
	}
	eventually(t, "a poisoned delivery to be dead-lettered", func() bool {
		h.apiOK(http.MethodGet, "/api/cqrs/deadletters", nil, &pending)
		return len(pending.DeadLetters) > 0
	})
	id := pending.DeadLetters[0].ID

	status, body = h.dash(http.MethodPost, "/consumers/deadletters/"+itoa64(id)+"/retry", nil, true)
	if status != http.StatusOK {
		t.Fatalf("retry: expected 200, got %d", status)
	}
	if strings.Contains(strings.ToLower(body), "<!doctype") {
		t.Error("a dead-letter action returned a page instead of the panel fragment")
	}
	if err := contains(body, `id="deadletters-panel"`, "still failing"); err != nil {
		t.Errorf("retry panel: %v", err)
	}

	// without htmx the same action redirects back to the page
	resp, _ = h.dashHeaders(http.MethodPost, "/consumers/deadletters/"+itoa64(id)+"/dismiss", nil, false)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/consumers" {
		t.Errorf("a plain form post should redirect to /consumers, got %d %q",
			resp.StatusCode, resp.Header.Get("Location"))
	}
}

// TestDashboardEditor drives the authoring workflow through the UI: dry run
// a candidate, watch a wrong contract get caught, save, and see the refusal
// path keep the operator's source.
func TestDashboardEditor(t *testing.T) {
	h := startBackend(t, map[string]string{"audit.js": fixedFn})
	h.startDashboard()
	h.login()
	h.command("task", "t1", "CreateTask", map[string]string{"title": "alpha"})
	h.command("task", "t2", "CreateTask", map[string]string{"title": "beta"})

	const wrong = `//@trigger projection ui_titles on TaskCreated
//@schema ui_titles title:text
//@key title
function project(event) { return [{ title: event.data.title }]; }
`
	status, body := h.dash(http.MethodPost, "/functions/act", formBody(map[string]string{
		"action": "dryrun", "name": "ui_titles.js", "mode": "projection", "source": wrong,
	}), true)
	if status != http.StatusOK {
		t.Fatalf("dry run: expected 200, got %d", status)
	}
	// the backend counts the values that are not row ops, so the panel names
	// the mistake instead of inferring it from a zero
	if err := contains(body, "Simulated", "are not row ops", "discarded at runtime"); err != nil {
		t.Errorf("the wrong-contract warning is missing: %v", err)
	}

	const right = `//@trigger projection ui_titles on TaskCreated
//@schema ui_titles title:text
//@key title
function project(event) { return [{ upsert: { key: event.data.title, fields: { title: event.data.title } } }]; }
`
	_, body = h.dash(http.MethodPost, "/functions/act", formBody(map[string]string{
		"action": "save", "name": "ui_titles.js", "source": right,
	}), true)
	if err := contains(body, "Saved, not live", "maintenance"); err != nil {
		t.Errorf("save: %v", err)
	}
	var read struct {
		Source string `json:"source"`
	}
	h.apiOK(http.MethodGet, "/api/cqrs/admin/functions/ui_titles.js", nil, &read)
	if !strings.Contains(read.Source, "upsert") {
		t.Fatal("the source saved through the UI is not what reached the backend")
	}

	// a refusal keeps the operator's source in the box and does not touch
	// the file on disk
	const broken = "//@trigger event TaskCreated\nfunction ((( broken\n"
	_, body = h.dash(http.MethodPost, "/functions/act", formBody(map[string]string{
		"action": "save", "name": "audit.js", "source": broken,
	}), true)
	if err := contains(body, "Not saved", "broken"); err != nil {
		t.Errorf("refused save: %v", err)
	}
	h.apiOK(http.MethodGet, "/api/cqrs/admin/functions/audit.js", nil, &read)
	if !strings.Contains(read.Source, "sink accepted") {
		t.Fatal("a refused save overwrote the file on disk")
	}

	// an empty name is refused rather than silently becoming new.js
	_, body = h.dash(http.MethodPost, "/functions/act", formBody(map[string]string{
		"action": "save", "name": "  ", "source": right,
	}), true)
	if !strings.Contains(body, "Give the file a name") {
		t.Error("an empty file name was accepted")
	}

	// one action per form: a per-button hx-post would fire the form's
	// request too, and the second response would win the swap
	_, page := h.dash(http.MethodGet, "/functions/audit.js", nil, false)
	if strings.Contains(page, "formaction=") {
		t.Error("formaction is back on a submit button: it fires the form's request as well")
	}
	if n := strings.Count(page, `hx-post="/functions/act"`); n != 1 {
		t.Errorf("the editor form should carry exactly one hx-post, found %d", n)
	}
}
