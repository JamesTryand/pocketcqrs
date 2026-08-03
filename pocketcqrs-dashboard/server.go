package main

import (
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
)

// authCookieName carries the PocketBase superuser token. HttpOnly (the
// browser never talks to pocketcqrs directly — all API calls are
// server-to-server), SameSite=Lax (top-level navigations keep the session).
const authCookieName = "pcqrs_auth"

type server struct {
	backend    *BackendClient
	backendURL string

	tmplLogin       *template.Template
	tmplOverview    *template.Template
	tmplPlaceholder *template.Template
}

func newServer(backendURL string) *server {
	parse := func(pages ...string) *template.Template {
		files := append([]string{"templates/layout.html"}, pages...)
		return template.Must(template.New("").ParseFS(templateFS, files...))
	}
	return &server{
		backend:         NewBackendClient(backendURL),
		backendURL:      strings.TrimSuffix(backendURL, "/"),
		tmplLogin:       parse("templates/login.html"),
		tmplOverview:    parse("templates/overview.html"),
		tmplPlaceholder: parse("templates/placeholder.html"),
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
	mux.HandleFunc("GET /aggregates", s.requireAuth(s.placeholder(
		"Aggregates", "/aggregates",
		"Coming in DASH.3 — streams and per-aggregate event browsing.",
	)))
	mux.HandleFunc("GET /events", s.requireAuth(s.placeholder(
		"Events", "/events",
		"Coming in DASH.3 — the event log browser with aggregate/type filters.",
	)))
	mux.HandleFunc("GET /consumers", s.requireAuth(s.placeholder(
		"Consumers", "/consumers",
		"Coming in DASH.3 — checkpoints, behind-by and dead letters.",
	)))
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
// side nav); handlers add their page-specific keys on top.
func (s *server) base(title, active string) map[string]any {
	return map[string]any{
		"Title":      title,
		"BackendURL": s.backendURL,
		"Nav":        navItems(active),
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
	data := map[string]any{
		"Title":      "Sign in",
		"BackendURL": s.backendURL,
		"Error":      errMsg,
	}
	if err := s.tmplLogin.ExecuteTemplate(w, "login", data); err != nil {
		log.Printf("render login: %v", err)
	}
}

// ---- pages ----

func (s *server) overview(w http.ResponseWriter, r *http.Request, token string) {
	cat, err := s.backend.Catalog(r.Context(), token)
	if errors.Is(err, ErrUnauthorized) {
		clearAuthCookie(w) // token expired or revoked — sign in again
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		s.renderPlaceholder(w, "Overview", "/", "Backend error",
			"Could not load the catalog: "+err.Error(), "danger")
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
	if err := s.tmplOverview.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render overview: %v", err)
	}
}

func (s *server) placeholder(title, active, text string) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, _ string) {
		s.renderPlaceholder(w, title, active, title, text, "brand")
	}
}

func (s *server) renderPlaceholder(w http.ResponseWriter, title, active, heading, text, variant string) {
	data := s.base(title, active)
	data["Heading"] = heading
	data["Text"] = text
	data["Variant"] = variant
	if err := s.tmplPlaceholder.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render placeholder: %v", err)
	}
}
