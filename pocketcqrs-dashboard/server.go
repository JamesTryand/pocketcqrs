package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// authCookieName carries the PocketBase superuser token. HttpOnly (the
// browser never talks to pocketcqrs directly — all API calls are
// server-to-server), SameSite=Lax (top-level navigations keep the session).
const authCookieName = "pcqrs_auth"

// defaultPageSize is the feed batch the browsing pages ask for. Small
// enough to render as cards, large enough that paging is rare.
const defaultPageSize = 50

// pages are the page templates; each is parsed together with layout.html.
// Fragment templates live inside their page's file (a polled table body is
// rendered by the same {{define}} whether it arrives with the page or on its
// own), so they need no separate entry here.
var pages = []string{
	"login", "overview", "placeholder",
	"aggregates", "streams", "stream", "events", "consumers", "system",
}

// livePoll is the hx-trigger interval of every self-refreshing panel, held
// in one place so a page and the fragment that replaces it cannot disagree.
//
// Both live panels are catalog-backed, and the catalog re-derives its log
// statistics on each call, so this is deliberately one dial rather than a
// per-panel guess — measured at ~4ms per fragment (dashboard + backend round
// trip, 20 requests) on the DASH.4 smoke instance, which two seconds carries
// comfortably. htmx 4 has no "stop polling" status code (2.x's 286 is
// gone from the 4.0.0-beta6 build): polling stops because the swapped-in
// element carries no hx-trigger, which is why every polled fragment renders
// its own trigger attributes.
const livePoll = "every 2s"

type server struct {
	backend    *BackendClient
	backendURL string

	tmpl map[string]*template.Template
}

func newServer(backendURL string) *server {
	tmpl := make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		tmpl[p] = template.Must(template.New("").ParseFS(templateFS,
			"templates/layout.html", "templates/"+p+".html"))
	}
	return &server{
		backend:    NewBackendClient(backendURL),
		backendURL: strings.TrimSuffix(backendURL, "/"),
		tmpl:       tmpl,
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	assets, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		log.Fatalf("assets: %v", err)
	}
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(assets)))
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent) // no icon yet; silence the 404
	})

	mux.HandleFunc("GET /login", s.loginForm)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /logout", s.logout)

	mux.HandleFunc("GET /{$}", s.requireAuth(s.overview))
	mux.HandleFunc("GET /aggregates", s.requireAuth(s.aggregates))
	mux.HandleFunc("GET /aggregates/{name}", s.requireAuth(s.aggregateStreams))
	mux.HandleFunc("GET /aggregates/{name}/{id}", s.requireAuth(s.stream))
	mux.HandleFunc("GET /events", s.requireAuth(s.events))
	mux.HandleFunc("GET /consumers", s.requireAuth(s.consumers))
	mux.HandleFunc("GET /system", s.requireAuth(s.system))

	// actions
	mux.HandleFunc("POST /system/mode", s.requireAuth(s.setMode))
	mux.HandleFunc("POST /system/reload", s.requireAuth(s.reload))
	mux.HandleFunc("POST /consumers/deadletters/retry", s.requireAuth(s.deadLetterRetryAll))
	mux.HandleFunc("POST /consumers/deadletters/{id}/retry", s.requireAuth(s.deadLetterRetry))
	mux.HandleFunc("POST /consumers/deadletters/{id}/dismiss", s.requireAuth(s.deadLetterDismiss))

	// htmx fragments: the same {{define}}s the pages render, on their own
	mux.HandleFunc("GET /fragments/overview", s.requireAuth(s.overviewFragment))
	mux.HandleFunc("GET /fragments/checkpoints", s.requireAuth(s.checkpointsFragment))
	mux.HandleFunc("GET /fragments/deadletters", s.requireAuth(s.deadLettersFragment))
	return mux
}

// isHTMX reports whether the request came from htmx rather than a plain
// browser navigation. htmx 4 sends HX-Request on every request it makes.
func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// ---- template data ----

type navItem struct {
	Label   string
	Href    string
	Icon    string // Web Awesome system-library icon (bundled, offline-safe)
	Current bool
}

func navItems(active string) []navItem {
	items := []navItem{
		{Label: "Overview", Href: "/", Icon: "gauge"},
		{Label: "Aggregates", Href: "/aggregates", Icon: "circle"},
		{Label: "Events", Href: "/events", Icon: "clock"},
		{Label: "Consumers", Href: "/consumers", Icon: "eye"},
		{Label: "System", Href: "/system", Icon: "gear"},
	}
	for i := range items {
		items[i].Current = items[i].Href == active
	}
	return items
}

// base returns the template data every page needs (title, backend link,
// side nav); handlers add their page-specific keys on top. Nested routes
// pass their section's href as active — nav items match by exact href.
func (s *server) base(title, active string) map[string]any {
	return map[string]any{
		"Title":      title,
		"BackendURL": s.backendURL,
		"Nav":        navItems(active),
	}
}

func (s *server) render(w http.ResponseWriter, page, root string, data any) {
	if err := s.tmpl[page].ExecuteTemplate(w, root, data); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

// ---- auth ----

func setAuthCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearAuthCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

type authedHandler func(http.ResponseWriter, *http.Request, string)

func (s *server) requireAuth(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(authCookieName)
		if err != nil || c.Value == "" {
			s.redirectToLogin(w, r)
			return
		}
		h(w, r, c.Value)
	}
}

// redirectToLogin sends the visitor back to the sign-in page.
//
// A plain navigation gets the usual 303. An htmx request must NOT: its fetch
// would follow the redirect transparently and swap the whole sign-in page
// into whatever element issued the request — for a polled table body, every
// two seconds, forever. htmx reads any hx-* RESPONSE header, so HX-Redirect
// navigates the actual browser instead (verified against the vendored
// 4.0.0-beta6 build, which derives those header names dynamically).
func (s *server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *server) loginForm(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(authCookieName); err == nil && c.Value != "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderLogin(w, http.StatusOK, "")
}

func (s *server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	token, err := s.backend.AuthWithPassword(r.Context(),
		r.PostFormValue("identity"), r.PostFormValue("password"))
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			s.renderLogin(w, http.StatusUnauthorized,
				"Sign-in failed — check the email and password, and that the account is a PocketBase superuser.")
			return
		}
		s.renderLogin(w, http.StatusBadGateway,
			"The pocketcqrs backend is unreachable at "+s.backendURL+" — is it serving?")
		return
	}
	setAuthCookie(w, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	clearAuthCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *server) renderLogin(w http.ResponseWriter, status int, errMsg string) {
	w.WriteHeader(status)
	s.render(w, "login", "login", map[string]any{
		"Title":      "Sign in",
		"BackendURL": s.backendURL,
		"Error":      errMsg,
	})
}

// backendOK handles the two failure modes every authed page shares: an
// expired or revoked token returns to the sign-in screen, anything else
// renders a 502 notice in place of the page. It reports whether the handler
// should carry on; when false, a response has already been written.
func (s *server) backendOK(w http.ResponseWriter, r *http.Request, title, active string, err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, ErrUnauthorized) {
		clearAuthCookie(w)
		s.redirectToLogin(w, r)
		return false
	}
	w.WriteHeader(http.StatusBadGateway)
	s.renderPlaceholder(w, title, active, "Backend error",
		"Could not reach the pocketcqrs backend: "+err.Error(), "danger")
	return false
}

// ---- feed paging ----

// eventFilter reads the feed filter out of the query string. Unparseable
// numbers fall back to their zero value, which the feed reads as "unbounded".
func eventFilter(r *http.Request) EventFilter {
	qv := r.URL.Query()
	f := EventFilter{
		Aggregate:   qv.Get("aggregate"),
		AggregateID: qv.Get("aggregateId"),
		Type:        qv.Get("type"),
		Limit:       defaultPageSize,
	}
	f.After, _ = strconv.ParseInt(qv.Get("after"), 10, 64)
	f.Before, _ = strconv.ParseInt(qv.Get("before"), 10, 64)
	if n, err := strconv.Atoi(qv.Get("limit")); err == nil && n > 0 {
		f.Limit = min(n, 1000)
	}
	return f
}

// paginate builds the Older/Newer links for a feed page, carrying the
// filters along. Results are ascending, so evs[0] is the oldest on screen.
// An empty link means "hide the control":
//
//   - a page with no bounds is the start of the log, so it has no Older;
//   - a short batch means that direction is exhausted;
//   - paging backwards drops After — it was only ever a floor guard.
func paginate(base string, f EventFilter, evs []Event) (older, newer string) {
	if len(evs) == 0 {
		return "", ""
	}
	full := len(evs) == f.Limit
	link := func(after, before int64) string {
		g := f
		g.After, g.Before = after, before
		return base + "?" + g.Query().Encode()
	}
	toOlder := link(0, evs[0].Position)
	toNewer := link(evs[len(evs)-1].Position, 0)

	switch {
	case f.After == 0 && f.Before == 0: // first page: at the log start
		if full {
			newer = toNewer
		}
	case f.Before > 0: // paged backwards
		if full {
			older = toOlder
		}
		newer = toNewer
	default: // paged forwards
		older = toOlder
		if full {
			newer = toNewer
		}
	}
	return older, newer
}

// ---- pages ----

func (s *server) overview(w http.ResponseWriter, r *http.Request, token string) {
	cat, err := s.backend.Catalog(r.Context(), token)
	if !s.backendOK(w, r, "Overview", "/", err) {
		return
	}

	raw, err := json.Marshal(cat) // encoding/json HTML-escapes by default
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		s.renderPlaceholder(w, "Overview", "/", "Render error", err.Error(), "danger")
		return
	}

	data := s.base("Overview", "/")
	liveOverview(data, cat)
	data["Mermaid"] = cat.Mermaid
	data["CatalogJSON"] = template.JS(raw)
	s.render(w, "overview", "layout", data)
}

// liveOverview fills the keys the self-refreshing overview panel reads. The
// catalog explorer is deliberately NOT part of that panel: re-laying out the
// graph every few seconds would be expensive and would discard whatever node
// the operator had selected.
func liveOverview(data map[string]any, cat *Catalog) {
	data["Poll"] = livePoll
	data["Mode"] = cat.Mode
	data["Totals"] = cat.Totals
	data["Aggregates"] = len(cat.Aggregates)
	data["Consumers"] = len(cat.Consumers)
	data["Generated"] = cat.GeneratedAt
}

// aggregates lists the registered aggregates and their empirical event types.
func (s *server) aggregates(w http.ResponseWriter, r *http.Request, token string) {
	cat, err := s.backend.Catalog(r.Context(), token)
	if !s.backendOK(w, r, "Aggregates", "/aggregates", err) {
		return
	}
	data := s.base("Aggregates", "/aggregates")
	data["Aggregates"] = cat.Aggregates
	s.render(w, "aggregates", "layout", data)
}

// aggregateStreams lists the streams of one aggregate.
func (s *server) aggregateStreams(w http.ResponseWriter, r *http.Request, token string) {
	name := r.PathValue("name")
	streams, err := s.backend.Streams(r.Context(), token, name)
	if !s.backendOK(w, r, name, "/aggregates", err) {
		return
	}
	data := s.base(name+" streams", "/aggregates")
	data["Aggregate"] = name
	data["Streams"] = streams
	s.render(w, "streams", "layout", data)
}

// stream shows one stream's events as a timeline, oldest first.
func (s *server) stream(w http.ResponseWriter, r *http.Request, token string) {
	name, id := r.PathValue("name"), r.PathValue("id")
	f := eventFilter(r)
	f.Aggregate, f.AggregateID = name, id

	evs, err := s.backend.Events(r.Context(), token, f)
	if !s.backendOK(w, r, name+"/"+id, "/aggregates", err) {
		return
	}
	older, newer := paginate("/aggregates/"+name+"/"+id, f, evs)

	data := s.base(name+"/"+id, "/aggregates")
	data["Aggregate"] = name
	data["StreamID"] = id
	data["Events"] = evs
	data["Older"] = older
	data["Newer"] = newer
	s.render(w, "stream", "layout", data)
}

// events is the log browser: the whole feed with filters and paging.
func (s *server) events(w http.ResponseWriter, r *http.Request, token string) {
	f := eventFilter(r)

	// the catalog supplies the aggregate and type options for the filter form
	cat, err := s.backend.Catalog(r.Context(), token)
	if !s.backendOK(w, r, "Events", "/events", err) {
		return
	}
	evs, err := s.backend.Events(r.Context(), token, f)
	if !s.backendOK(w, r, "Events", "/events", err) {
		return
	}
	older, newer := paginate("/events", f, evs)

	types := map[string]bool{}
	for _, a := range cat.Aggregates {
		for _, e := range a.Events {
			types[e.Type] = true
		}
	}

	data := s.base("Events", "/events")
	data["Events"] = evs
	data["Filter"] = f
	data["Aggregates"] = cat.Aggregates
	data["Types"] = sortedKeys(types)
	data["Older"] = older
	data["Newer"] = newer
	s.render(w, "events", "layout", data)
}

// consumerRow is one row of the consumers table: the catalog's consumer
// plus how far behind the head of the log it is.
type consumerRow struct {
	Consumer
	Behind int64
}

// consumers shows checkpoints, lag and the dead-letter queue.
func (s *server) consumers(w http.ResponseWriter, r *http.Request, token string) {
	includeResolved := r.URL.Query().Get("all") == "1"

	cat, err := s.backend.Catalog(r.Context(), token)
	if !s.backendOK(w, r, "Consumers", "/consumers", err) {
		return
	}
	letters, err := s.backend.DeadLetters(r.Context(), token, includeResolved)
	if !s.backendOK(w, r, "Consumers", "/consumers", err) {
		return
	}

	data := s.base("Consumers", "/consumers")
	checkpoints(data, cat)
	data["DeadLetters"] = letters
	data["IncludeResolved"] = includeResolved
	data["Flash"] = flashMessage{} // no action was taken to report
	s.render(w, "consumers", "layout", data)
}

// checkpoints fills the keys the polled checkpoints table body reads.
func checkpoints(data map[string]any, cat *Catalog) {
	rows := make([]consumerRow, 0, len(cat.Consumers))
	for _, c := range cat.Consumers {
		// behind-by is measured against the head of the log, never the
		// event count: checkpoints record a position
		behind := cat.Totals.MaxPosition - c.Checkpoint
		if behind < 0 {
			behind = 0 // a checkpoint ahead of the head means the log was reset
		}
		rows = append(rows, consumerRow{Consumer: c, Behind: behind})
	}
	data["Poll"] = livePoll
	data["Consumers"] = rows
	data["MaxPosition"] = cat.Totals.MaxPosition
	data["Pending"] = cat.Totals.DeadLettersPending
}

// ---- system: the mode barrier and hot reload ----

// system renders the mode toggle and the reload control. The two belong on
// one page because the barrier is an ordering rule between them: schema-
// bearing files only reload in maintenance, so the sequence is maintenance
// on → reload → maintenance off.
func (s *server) system(w http.ResponseWriter, r *http.Request, token string) {
	mode, err := s.backend.Mode(r.Context(), token)
	if !s.backendOK(w, r, "System", "/system", err) {
		return
	}
	s.render(w, "system", "layout", s.systemData(mode, nil, ""))
}

// systemData assembles the System page. report and reloadErr are the outcome
// of a reload just performed, if any.
func (s *server) systemData(mode string, report *ReloadReport, reloadErr string) map[string]any {
	data := s.base("System", "/system")
	data["Mode"] = mode
	data["Maintenance"] = mode == "maintenance"
	data["Report"] = report
	data["ReloadError"] = reloadErr
	return data
}

// setMode moves the barrier. This one is POST-then-redirect: the outcome is
// the new page state, so a refresh must not re-submit the toggle.
//
// The barrier is the safety-critical control on this page, so a refused
// toggle has to say that the TOGGLE was refused. A 4xx is the backend
// rejecting the value itself (it validates like store.SetMode and leaves the
// current mode untouched) — reported on the page, naming the value. Only an
// unreachable backend falls through to the generic notice; without the
// distinction an operator reads "Backend error" and cannot tell whether the
// system moved.
func (s *server) setMode(w http.ResponseWriter, r *http.Request, token string) {
	mode := r.PostFormValue("mode")
	_, err := s.backend.SetMode(r.Context(), token, mode)
	if err == nil {
		http.Redirect(w, r, "/system", http.StatusSeeOther)
		return
	}
	if errors.Is(err, ErrUnauthorized) {
		clearAuthCookie(w)
		s.redirectToLogin(w, r)
		return
	}

	var he *HTTPError
	if errors.As(err, &he) && he.StatusCode < 500 {
		current, merr := s.backend.Mode(r.Context(), token)
		if merr != nil {
			// the backend answered a moment ago, so this is unexpected;
			// fall back to the generic notice rather than guess a mode
			s.backendOK(w, r, "System", "/system", merr)
			return
		}
		data := s.systemData(current, nil, "")
		data["ModeError"] = fmt.Sprintf("The backend refused the mode %q, so the system is unchanged — it is still %s. (%s)",
			mode, current, he.Detail)
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "system", "layout", data)
		return
	}
	s.backendOK(w, r, "System", "/system", err)
}

// reload hot-reloads the backend's functions directory.
//
// Unlike the mode toggle this does NOT redirect: the report is the whole
// point of the action, and a redirect would throw it away. htmx swaps it
// into the report panel; without JS the same POST re-renders the page with
// the report in place.
func (s *server) reload(w http.ResponseWriter, r *http.Request, token string) {
	w.Header().Set("Vary", "HX-Request")

	report, err := s.backend.Reload(r.Context(), token)
	var reloadErr string
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			clearAuthCookie(w)
			s.redirectToLogin(w, r)
			return
		}
		// a file that fails to compile or validate is a 400 from the
		// backend, with the previous code left serving — that is a result
		// to report, not a dashboard failure
		reloadErr = err.Error()
	}

	mode := ""
	if report != nil {
		mode = report.Mode
	} else if m, merr := s.backend.Mode(r.Context(), token); merr == nil {
		mode = m
	}

	data := s.systemData(mode, report, reloadErr)
	if isHTMX(r) {
		s.render(w, "system", "reload-report", data)
		return
	}
	s.render(w, "system", "layout", data)
}

// ---- dead-letter actions ----

// deadLetterRetry re-delivers one dead letter through the backend's current
// function code.
func (s *server) deadLetterRetry(w http.ResponseWriter, r *http.Request, token string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid dead letter id", http.StatusBadRequest)
		return
	}
	res, err := s.backend.RetryDeadLetter(r.Context(), token, id)
	s.afterDeadLetterAction(w, r, token, retryFlash(res), err)
}

// deadLetterRetryAll re-delivers every pending dead letter.
func (s *server) deadLetterRetryAll(w http.ResponseWriter, r *http.Request, token string) {
	results, err := s.backend.RetryAllDeadLetters(r.Context(), token)

	flash := flashMessage{}
	if err == nil {
		resolved := 0
		for _, res := range results {
			if res.Resolved {
				resolved++
			}
		}
		switch {
		case len(results) == 0:
			flash = flashMessage{"neutral", "Nothing pending to retry."}
		case resolved == len(results):
			flash = flashMessage{"success", fmt.Sprintf("Re-delivered %d dead letters; all resolved.", resolved)}
		default:
			flash = flashMessage{"warning", fmt.Sprintf(
				"Re-delivered %d dead letters: %d resolved, %d still failing (see the error details).",
				len(results), resolved, len(results)-resolved)}
		}
	}
	s.afterDeadLetterAction(w, r, token, flash, err)
}

// deadLetterDismiss resolves a dead letter without re-delivering it.
func (s *server) deadLetterDismiss(w http.ResponseWriter, r *http.Request, token string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid dead letter id", http.StatusBadRequest)
		return
	}
	err = s.backend.DismissDeadLetter(r.Context(), token, id)
	s.afterDeadLetterAction(w, r, token,
		flashMessage{"neutral", fmt.Sprintf("Dead letter #%d dismissed without retrying.", id)}, err)
}

// flashMessage is the one-line outcome shown above the dead-letter table.
type flashMessage struct {
	Variant string
	Text    string
}

// retryFlash turns a retry outcome into its message. A retry that fails
// again is the ordinary case — the backend answers 200 with resolved=false —
// so it reads as a result, not as an error.
func retryFlash(res *DeadLetterResult) flashMessage {
	if res == nil {
		return flashMessage{}
	}
	if res.Resolved {
		return flashMessage{"success", fmt.Sprintf("Dead letter #%d re-delivered and resolved.", res.ID)}
	}
	return flashMessage{"danger", fmt.Sprintf(
		"Dead letter #%d still failing after %d attempts: %s", res.ID, res.Attempts, res.Error)}
}

// afterDeadLetterAction re-renders the dead-letter panel with the outcome.
// htmx swaps just that panel; a plain form post goes back to the page.
func (s *server) afterDeadLetterAction(w http.ResponseWriter, r *http.Request, token string, flash flashMessage, err error) {
	w.Header().Set("Vary", "HX-Request")

	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			clearAuthCookie(w)
			s.redirectToLogin(w, r)
			return
		}
		flash = flashMessage{"danger", "The action failed: " + err.Error()}
	}

	includeResolved := r.URL.Query().Get("all") == "1"
	if !isHTMX(r) {
		target := "/consumers"
		if includeResolved {
			target += "?all=1"
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	s.renderDeadLetterPanel(w, r, token, includeResolved, flash)
}

// ---- htmx fragments ----

// overviewFragment re-renders the mode banner and the totals cards.
func (s *server) overviewFragment(w http.ResponseWriter, r *http.Request, token string) {
	data := map[string]any{"Poll": livePoll}
	cat, err := s.backend.Catalog(r.Context(), token)
	if msg, handled := s.fragmentBackendErr(w, r, err); handled {
		return
	} else if msg != "" {
		data["Error"] = msg
	} else {
		liveOverview(data, cat)
	}
	s.render(w, "overview", "overview-live", data)
}

// checkpointsFragment re-renders the consumer checkpoints table body.
//
// Fragment=true lets the template add its out-of-band riders (the head-of-log
// and pending-dead-letters figures that live outside the table): inside the
// page's own <table> those elements would be invalid markup.
func (s *server) checkpointsFragment(w http.ResponseWriter, r *http.Request, token string) {
	data := map[string]any{"Poll": livePoll, "Fragment": true}
	cat, err := s.backend.Catalog(r.Context(), token)
	if msg, handled := s.fragmentBackendErr(w, r, err); handled {
		return
	} else if msg != "" {
		data["Error"] = msg
	} else {
		checkpoints(data, cat)
	}
	s.render(w, "consumers", "checkpoints-body", data)
}

// deadLettersFragment re-renders the dead-letter panel (the Refresh button).
func (s *server) deadLettersFragment(w http.ResponseWriter, r *http.Request, token string) {
	s.renderDeadLetterPanel(w, r, token, r.URL.Query().Get("all") == "1", flashMessage{})
}

func (s *server) renderDeadLetterPanel(w http.ResponseWriter, r *http.Request, token string, includeResolved bool, flash flashMessage) {
	data := map[string]any{"IncludeResolved": includeResolved, "Flash": flash}
	letters, err := s.backend.DeadLetters(r.Context(), token, includeResolved)
	if msg, handled := s.fragmentBackendErr(w, r, err); handled {
		return
	} else if msg != "" {
		data["Error"] = msg
	} else {
		data["DeadLetters"] = letters
	}
	s.render(w, "consumers", "deadletters-panel", data)
}

// fragmentBackendErr adjudicates a backend failure for a fragment response.
//
// An expired session is answered here and reported as handled: a fragment
// must never carry a whole sign-in page into a table body. Any other failure
// comes back as a message to render INSIDE the fragment — which keeps its own
// polling attributes, so the panel heals itself once the backend answers
// again. err == nil yields ("", false): render normally.
func (s *server) fragmentBackendErr(w http.ResponseWriter, r *http.Request, err error) (msg string, handled bool) {
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, ErrUnauthorized):
		clearAuthCookie(w)
		s.redirectToLogin(w, r)
		return "", true
	default:
		return "Could not reach the pocketcqrs backend: " + err.Error(), false
	}
}

func (s *server) renderPlaceholder(w http.ResponseWriter, title, active, heading, text, variant string) {
	data := s.base(title, active)
	data["Heading"] = heading
	data["Text"] = text
	data["Variant"] = variant
	s.render(w, "placeholder", "layout", data)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
