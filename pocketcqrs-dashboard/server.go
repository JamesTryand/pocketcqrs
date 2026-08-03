package main

import (
	"encoding/json"
	"errors"
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
var pages = []string{
	"login", "overview", "placeholder",
	"aggregates", "streams", "stream", "events", "consumers",
}

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
	return mux
}

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
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h(w, r, c.Value)
	}
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
		http.Redirect(w, r, "/login", http.StatusSeeOther)
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
	data["Mode"] = cat.Mode
	data["Totals"] = cat.Totals
	data["Aggregates"] = len(cat.Aggregates)
	data["Consumers"] = len(cat.Consumers)
	data["Generated"] = cat.GeneratedAt
	data["Mermaid"] = cat.Mermaid
	data["CatalogJSON"] = template.JS(raw)
	s.render(w, "overview", "layout", data)
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

	data := s.base("Consumers", "/consumers")
	data["Consumers"] = rows
	data["MaxPosition"] = cat.Totals.MaxPosition
	data["DeadLetters"] = letters
	data["IncludeResolved"] = includeResolved
	s.render(w, "consumers", "layout", data)
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
