package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// BackendClient talks to a pocketcqrs instance over its PUBLIC HTTP API
// only. The dashboard is an external dogfood consumer: it deliberately
// does not import any pocketcqrs package, so the JSON shapes below are the
// API contract exactly as an out-of-process consumer would see it.
type BackendClient struct {
	base string
	hc   *http.Client
}

// NewBackendClient targets the pocketcqrs instance at base
// (e.g. http://127.0.0.1:8090).
func NewBackendClient(base string) *BackendClient {
	return &BackendClient{
		base: strings.TrimSuffix(base, "/"),
		hc:   &http.Client{Timeout: 15 * time.Second},
	}
}

// ErrUnauthorized marks a 401/403 from the backend (expired or invalid
// superuser token) — or a failed auth-with-password (PocketBase answers
// failed logins with 400).
var ErrUnauthorized = errors.New("backend: unauthorized")

// HTTPError is any other non-2xx backend response.
type HTTPError struct {
	StatusCode int
	Detail     string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("backend: %d: %s", e.StatusCode, e.Detail)
}

func (c *BackendClient) do(ctx context.Context, method, path, token string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("backend unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &HTTPError{StatusCode: resp.StatusCode, Detail: strings.TrimSpace(string(snippet))}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decoding backend response: %w", err)
		}
	}
	return nil
}

// AuthWithPassword signs a superuser in through PocketBase's public auth
// endpoint and returns the auth token. Failed credentials surface as
// ErrUnauthorized (PocketBase answers them with 400, not 401).
func (c *BackendClient) AuthWithPassword(ctx context.Context, identity, password string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := c.do(ctx, http.MethodPost, "/api/collections/_superusers/auth-with-password", "",
		map[string]string{"identity": identity, "password": password}, &out)
	var he *HTTPError
	if errors.As(err, &he) && he.StatusCode == http.StatusBadRequest {
		return "", ErrUnauthorized
	}
	if err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", errors.New("backend returned no token")
	}
	return out.Token, nil
}

// Catalog fetches the platform catalog (GET /api/cqrs/catalog).
func (c *BackendClient) Catalog(ctx context.Context, token string) (*Catalog, error) {
	var cat Catalog
	if err := c.do(ctx, http.MethodGet, "/api/cqrs/catalog", token, nil, &cat); err != nil {
		return nil, err
	}
	return &cat, nil
}

// Streams lists one row per stream (GET /api/cqrs/streams). An empty
// aggregate lists streams of every aggregate type.
func (c *BackendClient) Streams(ctx context.Context, token, aggregate string) ([]StreamInfo, error) {
	q := url.Values{}
	if aggregate != "" {
		q.Set("aggregate", aggregate)
	}
	var out struct {
		Streams []StreamInfo `json:"streams"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/cqrs/streams?"+q.Encode(), token, nil, &out); err != nil {
		return nil, err
	}
	return out.Streams, nil
}

// EventFilter is the query behind GET /api/cqrs/events. Zero values are
// omitted, so the zero filter is "the first page of the whole log".
type EventFilter struct {
	Aggregate   string
	AggregateID string
	Type        string
	After       int64
	Before      int64
	Limit       int
}

// Query renders the filter as the feed's query string. Kept separate from
// Events so page templates can reuse it for pagination links.
func (f EventFilter) Query() url.Values {
	q := url.Values{}
	if f.Aggregate != "" {
		q.Set("aggregate", f.Aggregate)
	}
	if f.AggregateID != "" {
		q.Set("aggregateId", f.AggregateID)
	}
	if f.Type != "" {
		q.Set("type", f.Type)
	}
	if f.After > 0 {
		q.Set("after", strconv.FormatInt(f.After, 10))
	}
	if f.Before > 0 {
		q.Set("before", strconv.FormatInt(f.Before, 10))
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	return q
}

// Events reads the log feed (GET /api/cqrs/events). Results always come
// back in ascending position order, whichever direction was paged.
func (c *BackendClient) Events(ctx context.Context, token string, f EventFilter) ([]Event, error) {
	var out struct {
		Events []Event `json:"events"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/cqrs/events?"+f.Query().Encode(), token, nil, &out); err != nil {
		return nil, err
	}
	return out.Events, nil
}

// DeadLetters lists failed function deliveries (GET /api/cqrs/deadletters),
// pending only unless includeResolved.
func (c *BackendClient) DeadLetters(ctx context.Context, token string, includeResolved bool) ([]DeadLetter, error) {
	path := "/api/cqrs/deadletters"
	if includeResolved {
		path += "?all=1"
	}
	var out struct {
		DeadLetters []DeadLetter `json:"deadLetters"`
	}
	if err := c.do(ctx, http.MethodGet, path, token, nil, &out); err != nil {
		return nil, err
	}
	return out.DeadLetters, nil
}

// Mode reads the system mode barrier (GET /api/cqrs/admin/mode).
func (c *BackendClient) Mode(ctx context.Context, token string) (string, error) {
	var out struct {
		Mode string `json:"mode"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/cqrs/admin/mode", token, nil, &out); err != nil {
		return "", err
	}
	return out.Mode, nil
}

// SetMode moves the barrier (POST /api/cqrs/admin/mode). The backend
// validates the value, so an unknown mode comes back as an HTTPError and
// the current mode is unchanged.
func (c *BackendClient) SetMode(ctx context.Context, token, mode string) (string, error) {
	var out struct {
		Mode string `json:"mode"`
	}
	err := c.do(ctx, http.MethodPost, "/api/cqrs/admin/mode", token,
		map[string]string{"mode": mode}, &out)
	if err != nil {
		return "", err
	}
	return out.Mode, nil
}

// Reload hot-reloads the backend's functions directory
// (POST /api/cqrs/admin/reload) and returns its report. A file that fails to
// compile or validate is a 400 with the previous code left serving — that
// arrives here as an *HTTPError carrying the backend's message.
func (c *BackendClient) Reload(ctx context.Context, token string) (*ReloadReport, error) {
	var rep ReloadReport
	if err := c.do(ctx, http.MethodPost, "/api/cqrs/admin/reload", token, nil, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// RetryDeadLetter re-delivers one dead letter through the backend's current
// function code. A retry that fails again is a successful call reporting
// Resolved=false — see DeadLetterResult.
func (c *BackendClient) RetryDeadLetter(ctx context.Context, token string, id int64) (*DeadLetterResult, error) {
	var out DeadLetterResult
	path := "/api/cqrs/deadletters/" + strconv.FormatInt(id, 10) + "/retry"
	if err := c.do(ctx, http.MethodPost, path, token, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RetryAllDeadLetters re-delivers every pending dead letter, oldest first.
func (c *BackendClient) RetryAllDeadLetters(ctx context.Context, token string) ([]DeadLetterResult, error) {
	var out struct {
		Results []DeadLetterResult `json:"results"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/cqrs/deadletters/retry", token, nil, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// DismissDeadLetter resolves a dead letter without re-delivering it.
func (c *BackendClient) DismissDeadLetter(ctx context.Context, token string, id int64) error {
	path := "/api/cqrs/deadletters/" + strconv.FormatInt(id, 10) + "/dismiss"
	return c.do(ctx, http.MethodPost, path, token, nil, nil)
}

// FunctionFiles lists the backend's functions directory.
func (c *BackendClient) FunctionFiles(ctx context.Context, token string) ([]FunctionFile, string, error) {
	var out struct {
		Dir   string         `json:"dir"`
		Files []FunctionFile `json:"files"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/cqrs/admin/functions", token, nil, &out); err != nil {
		return nil, "", err
	}
	return out.Files, out.Dir, nil
}

// FunctionSource reads one function file.
func (c *BackendClient) FunctionSource(ctx context.Context, token, name string) (*FunctionFile, error) {
	var out FunctionFile
	if err := c.do(ctx, http.MethodGet, "/api/cqrs/admin/functions/"+url.PathEscape(name), token, nil, &out); err != nil {
		return nil, err
	}
	out.Name = name
	return &out, nil
}

// PreviousFunctionSource reads the copy the backend kept when this file was
// last overwritten. ErrNotFound-shaped 404s mean there has been no overwrite.
func (c *BackendClient) PreviousFunctionSource(ctx context.Context, token, name string) (string, error) {
	var out FunctionFile
	path := "/api/cqrs/admin/functions/" + url.PathEscape(name) + "/previous"
	if err := c.do(ctx, http.MethodGet, path, token, nil, &out); err != nil {
		return "", err
	}
	return out.Source, nil
}

// SaveFunction writes a function file. The backend refuses source that does
// not load, so a 400 here carries the parse or compile error.
func (c *BackendClient) SaveFunction(ctx context.Context, token, name, source string) (*SaveResult, error) {
	var out SaveResult
	err := c.do(ctx, http.MethodPut, "/api/cqrs/admin/functions/"+url.PathEscape(name), token,
		map[string]string{"source": source}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteFunction removes a function file. Whatever it registered keeps
// serving until the next reload drops it.
func (c *BackendClient) DeleteFunction(ctx context.Context, token, name string) (*SaveResult, error) {
	var out SaveResult
	err := c.do(ctx, http.MethodDelete, "/api/cqrs/admin/functions/"+url.PathEscape(name), token, nil, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Scaffold generates a slice's source from a domain description. It writes
// nothing: the files come back for review, dry run and save through the
// ordinary function-file API.
func (c *BackendClient) Scaffold(ctx context.Context, token string, domain any) ([]GeneratedFile, []string, error) {
	var out struct {
		Files []GeneratedFile `json:"files"`
		// Warnings name what the description left unfinished — a payload
		// nobody specified, a command with more than one possible event.
		// Generated code carries the same note in a comment, which is
		// exactly where it would be missed.
		Warnings []string `json:"warnings"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/cqrs/admin/scaffold", token, domain, &out); err != nil {
		return nil, nil, err
	}
	return out.Files, out.Warnings, nil
}

// GeneratedFile is one file the scaffolder produced, with the dry-run mode
// its kind calls for.
type GeneratedFile struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Kind   string `json:"kind"`
}

// DryRunMode is the check that applies to this generated file.
func (f GeneratedFile) DryRunMode() string {
	switch f.Kind {
	case "decider", "projection":
		return f.Kind
	default:
		return "compile"
	}
}

// DryRun runs candidate source against real history without persisting
// anything (POST /api/cqrs/admin/dryrun).
func (c *BackendClient) DryRun(ctx context.Context, token string, req DryRunRequest) (*DryRunResult, error) {
	var out DryRunResult
	if err := c.do(ctx, http.MethodPost, "/api/cqrs/admin/dryrun", token, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---- the function-file API contract ----

// Declaration is what a function file's directives declare, as the backend's
// loader reads them.
type Declaration struct {
	Kind        string   `json:"kind"` // effect | projection | decider | none
	EventTypes  []string `json:"eventTypes,omitempty"`
	HTTP        bool     `json:"http,omitempty"`
	Cron        string   `json:"cron,omitempty"`
	Projection  string   `json:"projection,omitempty"`
	Collections []string `json:"collections,omitempty"`
	Aggregate   string   `json:"aggregate,omitempty"`
	Handles     []string `json:"handles,omitempty"`
	// SchemaBearing reports whether activating the file needs maintenance
	// mode — projection schemas and deciders move only behind the barrier.
	SchemaBearing bool `json:"schemaBearing"`
}

// DryRunMode is the check to run for a file of this kind. The mode is chosen
// from the declaration rather than guessed by the backend, so the page can
// name the check it is about to run.
func (d *Declaration) DryRunMode() string {
	if d == nil {
		return "compile"
	}
	switch d.Kind {
	case "decider":
		return "decider"
	case "projection":
		return "projection"
	case "reactor":
		return "react"
	default:
		return "compile"
	}
}

// FunctionFile is one entry of the function listing (Source is set only when
// a single file is read).
type FunctionFile struct {
	Name        string       `json:"name"`
	Size        int64        `json:"size"`
	Modified    string       `json:"modified"`
	Source      string       `json:"source,omitempty"`
	Declaration *Declaration `json:"declaration,omitempty"`
	Error       string       `json:"error,omitempty"`
	// HasPrevious reports that the backend kept a copy from the last
	// overwrite, so the editor can offer to load it back.
	HasPrevious bool `json:"hasPrevious,omitempty"`
}

// Kind reports the file's declared kind, or "unreadable" when it does not
// parse — a file that blocks every reload until it is fixed or removed.
func (f FunctionFile) Kind() string {
	if f.Declaration == nil {
		return "unreadable"
	}
	return f.Declaration.Kind
}

// SaveResult is the answer to a write or delete. Active is always false:
// saving is not activating.
type SaveResult struct {
	Name        string       `json:"name"`
	Declaration *Declaration `json:"declaration,omitempty"`
	Active      bool         `json:"active"`
	Deleted     bool         `json:"deleted,omitempty"`
	Hint        string       `json:"hint"`
}

// DryRunRequest is the body of POST /api/cqrs/admin/dryrun.
type DryRunRequest struct {
	Name     string          `json:"name"`
	Source   string          `json:"source"`
	Mode     string          `json:"mode"`
	StreamID string          `json:"streamId,omitempty"`
	Command  string          `json:"command,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Diff     bool            `json:"diff,omitempty"`
}

// DryRunResult is what a dry run reports. Fields not relevant to the mode
// are absent.
type DryRunResult struct {
	Mode        string       `json:"mode"`
	OK          bool         `json:"ok"`
	Summary     string       `json:"summary"`
	Declaration *Declaration `json:"declaration,omitempty"`
	Aggregate   string       `json:"aggregate,omitempty"`
	Streams     int          `json:"streams,omitempty"`
	// Events is how much history was folded (decider/projection modes).
	Events int `json:"events,omitempty"`
	// Produced is what a command WOULD append (decide mode) — a separate
	// field from Events, which is a count.
	Produced json.RawMessage `json:"produced,omitempty"`
	// Reactor and Dispatches report a react-mode dry run: the commands the
	// reactor WOULD send. Named separately from Produced for the same
	// reason Produced is separate from Events — one field must not mean a
	// different shape per mode.
	Reactor    string `json:"reactor,omitempty"`
	Dispatches []struct {
		CausedBy  int64           `json:"causedBy"`
		Aggregate string          `json:"aggregate"`
		ID        string          `json:"id"`
		Command   string          `json:"command"`
		Payload   json.RawMessage `json:"payload,omitempty"`
	} `json:"dispatches,omitempty"`
	// Rejected reports that the decider REFUSED the command (decide mode).
	// It arrives on a 200 with OK false, because the dry run itself worked:
	// a working decider giving a domain answer must not look like a broken
	// endpoint. Branch on this, never on the status code.
	Rejected bool `json:"rejected,omitempty"`
	// Message is the rejection's reason. Only set when Rejected.
	Message     string `json:"message,omitempty"`
	Name        string `json:"name,omitempty"`
	Upserts     int    `json:"upserts,omitempty"`
	Deletes     int    `json:"deletes,omitempty"`
	Rows        int    `json:"rows,omitempty"`
	Collections int    `json:"collections,omitempty"`
	// IgnoredValues counts values project() returned that are not row ops
	// and would be discarded at runtime.
	IgnoredValues int                 `json:"ignoredValues,omitempty"`
	State         any                 `json:"state,omitempty"`
	Diff          map[string][]string `json:"diff,omitempty"`
}

// Detail renders the mode-specific part of a dry-run result for display:
// the events a command would produce, or the folded state.
func (r DryRunResult) Detail() string {
	if len(r.Produced) > 0 {
		return indentJSON(r.Produced)
	}
	if r.State != nil {
		raw, err := json.MarshalIndent(r.State, "", "  ")
		if err != nil {
			return ""
		}
		return string(raw)
	}
	return ""
}

// ---- the public catalog JSON contract (mirror of the API document) ----

type Catalog struct {
	GeneratedAt string       `json:"generatedAt"`
	Mode        string       `json:"mode"`
	Totals      Totals       `json:"totals"`
	Aggregates  []Aggregate  `json:"aggregates"`
	Consumers   []Consumer   `json:"consumers"`
	Collections []Collection `json:"collections"`
	Functions   Functions    `json:"functions"`
	Flows       []Flow       `json:"flows"`
	Mermaid     string       `json:"mermaid"`
}

type Totals struct {
	Events int64 `json:"events"`
	// MaxPosition is the head of the log — the base for behind-by, since
	// checkpoints record a position rather than a count.
	MaxPosition        int64 `json:"maxPosition"`
	Streams            int64 `json:"streams"`
	DeadLettersPending int64 `json:"deadLettersPending"`
}

type Aggregate struct {
	Name       string      `json:"name"`
	Origin     string      `json:"origin"`
	Handles    []string    `json:"handles,omitempty"`
	Transforms []string    `json:"transforms,omitempty"`
	Streams    int64       `json:"streams"`
	Events     []EventType `json:"events"`
}

type EventType struct {
	Type       string `json:"type"`
	Count      int64  `json:"count"`
	MinVersion int64  `json:"minVersion"`
	MaxVersion int64  `json:"maxVersion"`
}

type Consumer struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	EventTypes  []string `json:"eventTypes,omitempty"`
	Collections []string `json:"collections,omitempty"`
	Checkpoint  int64    `json:"checkpoint"`
}

type Collection struct {
	Name    string   `json:"name"`
	Guarded bool     `json:"guarded"`
	Owner   string   `json:"owner"`
	Key     string   `json:"key,omitempty"`
	Fields  []string `json:"fields"`
}

type Functions struct {
	HTTP []string  `json:"http"`
	Cron []CronJob `json:"cron"`
}

type CronJob struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
}

type Flow struct {
	Reactor string `json:"reactor"`
	Cause   string `json:"cause"`
	Target  string `json:"target"`
	Count   int64  `json:"count"`
}

// ---- the operational API JSON contract (log feed, streams, dead letters) ----

// Event is one committed event envelope as the feed hands it out (already
// upcast to its latest schema version by the backend).
type Event struct {
	Position    int64           `json:"position"`
	ID          string          `json:"id"`
	Aggregate   string          `json:"aggregate"`
	AggregateID string          `json:"aggregateId"`
	Sequence    int64           `json:"sequence"`
	Type        string          `json:"type"`
	Data        json.RawMessage `json:"data"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Version     int64           `json:"version"`
	Created     string          `json:"created"`
}

// Actor reports who caused the event, from the envelope metadata
// ("reactor:<name>" for reactions, an auth record id for commands). Returns
// "" when the metadata carries no actor. html/template cannot index into
// raw JSON, so pages call this rather than reaching into Metadata.
func (e Event) Actor() string {
	if len(e.Metadata) == 0 {
		return ""
	}
	var meta struct {
		Actor string `json:"actor"`
	}
	if err := json.Unmarshal(e.Metadata, &meta); err != nil {
		return ""
	}
	return meta.Actor
}

// Payload renders the event body as indented JSON for display. Templates
// cannot print a json.RawMessage usefully (it is a []byte), so pages call
// this instead of reaching into Data.
func (e Event) Payload() string { return indentJSON(e.Data) }

// MetadataJSON renders the envelope metadata the same way as Payload.
func (e Event) MetadataJSON() string { return indentJSON(e.Metadata) }

func indentJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw) // not valid JSON — show it as it arrived
	}
	return buf.String()
}

// ReloadReport is what POST /api/cqrs/admin/reload hands back: what was
// swapped, and whether the schema-bearing tier was allowed through the
// maintenance barrier.
type ReloadReport struct {
	Mode            string   `json:"mode"`
	EffectsReloaded []string `json:"effectsReloaded"`
	HTTPReloaded    []string `json:"httpReloaded"`
	CronReloaded    []string `json:"cronReloaded"`
	// SchemaTier is "reloaded" or "skipped: not in maintenance".
	SchemaTier         string   `json:"schemaTier"`
	Projections        []string `json:"projectionsReloaded,omitempty"`
	ProjectionsRemoved []string `json:"projectionsRemoved,omitempty"`
	DecidersReloaded   []string `json:"decidersReloaded,omitempty"`
	DecidersRemoved    []string `json:"decidersRemoved,omitempty"`
	DecidersRefused    []string `json:"decidersRefused,omitempty"`
}

// SchemaSkipped reports whether the schema-bearing tier was held back by the
// barrier — i.e. the reload ran while the system was still running.
func (r ReloadReport) SchemaSkipped() bool {
	return strings.HasPrefix(r.SchemaTier, "skipped")
}

// DeadLetterResult is the outcome of one retry or dismissal. Resolved=false
// with an Error is the ordinary "still poison" case, not a failed call.
type DeadLetterResult struct {
	ID       int64  `json:"id"`
	Consumer string `json:"consumer,omitempty"`
	Resolved bool   `json:"resolved"`
	Attempts int64  `json:"attempts,omitempty"`
	Error    string `json:"error,omitempty"`
}

// StreamInfo describes one stream: length, head position, last write.
type StreamInfo struct {
	Aggregate    string `json:"aggregate"`
	AggregateID  string `json:"aggregateId"`
	Events       int64  `json:"events"`
	LastPosition int64  `json:"lastPosition"`
	Updated      string `json:"updated"`
}

// DeadLetter is a failed function delivery, captured with the event that
// caused it.
type DeadLetter struct {
	ID          int64  `json:"id"`
	Consumer    string `json:"consumer"`
	EventPos    int64  `json:"eventPos"`
	Event       Event  `json:"event"`
	Error       string `json:"error"`
	Attempts    int64  `json:"attempts"`
	FirstFailed string `json:"firstFailed"`
	LastFailed  string `json:"lastFailed"`
	Resolved    bool   `json:"resolved"`
}
