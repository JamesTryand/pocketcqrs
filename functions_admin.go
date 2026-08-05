package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/functions"
	"github.com/jamestryand/pocketcqrs/scaffold"
)

// functionNamePattern is the whole allowed shape of a function file name:
// one path segment, .js, no directory separators and no leading dot.
var functionNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.js$`)

// windowsReservedNames are legacy DOS device names. Windows resolves them as
// devices whatever the extension, so "CON.js" is not a file — a pattern
// match alone would let one through on the platform this runs on.
var windowsReservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// resolveFunctionPath maps a caller-supplied file name to a path inside dir.
//
// This is the entire control surface of the function-file API: everything
// behind it writes code that the server will execute. The name is validated
// by pattern AND the resolved path is checked to still be a direct child of
// dir, so a traversal that slips past the pattern cannot escape anyway.
func resolveFunctionPath(dir, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("function file name is required")
	}
	if !functionNamePattern.MatchString(name) {
		return "", fmt.Errorf("invalid function file name %q: expected a single .js file name (letters, digits, dot, dash, underscore)", name)
	}
	if windowsReservedNames[strings.ToLower(strings.TrimSuffix(name, ".js"))] {
		return "", fmt.Errorf("invalid function file name %q: reserved device name", name)
	}
	full := filepath.Clean(filepath.Join(dir, name))
	if filepath.Dir(full) != filepath.Clean(dir) {
		return "", fmt.Errorf("invalid function file name %q: resolves outside the functions directory", name)
	}
	return full, nil
}

// functionFile is one entry of the function-file listing.
type functionFile struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	// Declaration is what the file's directives declare, as the loader reads
	// them; nil when the file does not parse (Error then says why).
	Declaration *functions.Declaration `json:"declaration,omitempty"`
	Error       string                 `json:"error,omitempty"`
	// HasPrevious reports that a copy was kept when this file was last
	// overwritten, so a caller can offer to load it back.
	HasPrevious bool `json:"hasPrevious,omitempty"`
}

// registerFunctionAdminRoutes binds the superuser-only function-file API:
//
//	GET    /api/cqrs/admin/functions          list pb_functions/*.js
//	GET    /api/cqrs/admin/functions/{name}   read one file's source
//	PUT    /api/cqrs/admin/functions/{name}   write it   body: {"source":"..."}
//	DELETE /api/cqrs/admin/functions/{name}   remove it
//	POST   /api/cqrs/admin/dryrun             run candidate source against real history
//
// TRUST MODEL: these routes write code that the server executes in-process
// (goja). That is deliberate and unchanged from the rest of the platform —
// functions are trusted, owner-authored code, `pack import` already writes
// the same files, and every route here requires a superuser token. Untrusted
// function code remains the open question that a wasm runtime would answer
// (see NEEDS.md).
//
// A write is refused unless the source loads: a file that cannot be parsed
// or compiled would abort EVERY later reload (reloads are all-or-nothing),
// so one bad save would block the fix for it. Nothing written here is live
// until POST /api/cqrs/admin/reload — and schema-bearing files need the
// maintenance barrier first.
func registerFunctionAdminRoutes(e *core.ServeEvent, c *components, functionsDir string) {
	e.Router.GET("/api/cqrs/admin/functions", func(re *core.RequestEvent) error {
		files, err := listFunctionFiles(functionsDir)
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		return re.JSON(http.StatusOK, map[string]any{"dir": functionsDir, "files": files})
	}).Bind(apis.RequireSuperuserAuth())

	e.Router.GET("/api/cqrs/admin/functions/{name}", func(re *core.RequestEvent) error {
		path, err := resolveFunctionPath(functionsDir, re.Request.PathValue("name"))
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return apis.NewNotFoundError("no such function file", err)
		}
		name := filepath.Base(path)
		out := map[string]any{"name": name, "source": string(raw)}
		if _, err := os.Stat(path + previousSuffix); err == nil {
			out["hasPrevious"] = true
		}
		if d, derr := functions.Declares(name, string(raw)); derr == nil {
			out["declaration"] = d
		} else {
			out["error"] = derr.Error()
		}
		return re.JSON(http.StatusOK, out)
	}).Bind(apis.RequireSuperuserAuth())

	// the copy kept when this file was last overwritten — the undo a bare
	// os.WriteFile does not give you
	e.Router.GET("/api/cqrs/admin/functions/{name}/previous", func(re *core.RequestEvent) error {
		path, err := resolveFunctionPath(functionsDir, re.Request.PathValue("name"))
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		raw, err := os.ReadFile(path + previousSuffix)
		if err != nil {
			return apis.NewNotFoundError("no previous version of this function file", err)
		}
		return re.JSON(http.StatusOK, map[string]any{
			"name": filepath.Base(path), "source": string(raw),
		})
	}).Bind(apis.RequireSuperuserAuth())

	e.Router.PUT("/api/cqrs/admin/functions/{name}", func(re *core.RequestEvent) error {
		path, err := resolveFunctionPath(functionsDir, re.Request.PathValue("name"))
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		payload, err := io.ReadAll(re.Request.Body)
		if err != nil {
			return apis.NewBadRequestError("failed reading request body", err)
		}
		var body struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return apis.NewBadRequestError("invalid JSON body: "+err.Error(), err)
		}
		name := filepath.Base(path)

		// load-check before writing: an unloadable file in the directory
		// would abort every later reload, including the one that fixes it
		decl, err := c.checkFunctionSource(name, body.Source)
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}

		// writes take the reload lock: a reload reads the whole directory,
		// and a write landing mid-read would tear the load
		c.reloadMu.Lock()
		replaced, err := keepPreviousVersion(path)
		if err == nil {
			err = os.WriteFile(path, []byte(body.Source), 0o644)
		}
		c.reloadMu.Unlock()
		if err != nil {
			return apis.NewBadRequestError("failed writing the function file: "+err.Error(), err)
		}
		return re.JSON(http.StatusOK, map[string]any{
			"name":        name,
			"declaration": decl,
			"active":      false,
			"hasPrevious": replaced,
			"hint":        activationHint(decl),
		})
	}).Bind(apis.RequireSuperuserAuth())

	e.Router.DELETE("/api/cqrs/admin/functions/{name}", func(re *core.RequestEvent) error {
		path, err := resolveFunctionPath(functionsDir, re.Request.PathValue("name"))
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		c.reloadMu.Lock()
		err = os.Remove(path)
		c.reloadMu.Unlock()
		if err != nil {
			return apis.NewNotFoundError("no such function file", err)
		}
		return re.JSON(http.StatusOK, map[string]any{
			"name":    filepath.Base(path),
			"deleted": true,
			"hint":    "The file is gone, but whatever it registered is still serving until the next reload drops it.",
		})
	}).Bind(apis.RequireSuperuserAuth())

	e.Router.POST("/api/cqrs/admin/dryrun", func(re *core.RequestEvent) error {
		return c.handleDryRun(re)
	}).Bind(apis.RequireSuperuserAuth())

	// Generate a slice's source from a domain description. It writes
	// nothing: the caller saves the files through PUT above, so generated
	// code goes through the same load check, dry run and maintenance barrier
	// as anything hand-written. Generated code gets no shortcut.
	e.Router.POST("/api/cqrs/admin/scaffold", func(re *core.RequestEvent) error {
		payload, err := io.ReadAll(re.Request.Body)
		if err != nil {
			return apis.NewBadRequestError("failed reading request body", err)
		}
		var domain scaffold.Domain
		if err := json.Unmarshal(payload, &domain); err != nil {
			return apis.NewBadRequestError("invalid JSON body: "+err.Error(), err)
		}
		files, err := domain.Generate()
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		return re.JSON(http.StatusOK, map[string]any{
			"files": files,
			"hint": "Nothing was written. Dry-run each file, save it, then reload — " +
				"a generated decider and projection are a starting point, not a finished domain.",
		})
	}).Bind(apis.RequireSuperuserAuth())
}

// previousSuffix marks the copy kept when a file is overwritten. It is not
// a .js file, so the loader ignores it and the listing does not show it.
const previousSuffix = ".prev"

// keepPreviousVersion copies the file about to be overwritten aside, and
// reports whether there was one.
//
// A save through the API is otherwise a bare overwrite with no undo. In this
// repo the function files happen to be version-controlled; in a deployment —
// which is exactly where the editor is used — the functions directory
// usually is not, so a mis-paste is unrecoverable. One previous version is
// not history, but it turns "gone" into "one click back".
func keepPreviousVersion(path string) (bool, error) {
	current, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // a new file has no previous version
		}
		return false, err
	}
	if err := os.WriteFile(path+previousSuffix, current, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// activationHint says what it takes to make a saved file live. Saving is
// never activation: the barrier is the point.
func activationHint(d *functions.Declaration) string {
	if d != nil && d.SchemaBearing {
		return "Saved, not live. This file is schema-bearing: enter maintenance mode, reload, then return to running."
	}
	return "Saved, not live. Reload to activate it; effect, HTTP and cron functions reload in any mode."
}

// checkFunctionSource loads candidate source the way the loader would,
// without registering anything, and reports what it declares.
func (c *components) checkFunctionSource(name, src string) (*functions.Declaration, error) {
	decl, err := functions.Declares(name, src)
	if err != nil {
		return nil, err
	}
	// a scratch runtime: compiling into the live one would half-register
	// code that has not been through the barrier
	rt := functions.NewGojaRuntime(func(string, ...any) {})
	switch decl.Kind {
	case functions.KindDecider:
		if _, err := functions.LoadDeciderSource(rt, name, src); err != nil {
			return nil, err
		}
	case functions.KindProjection:
		if _, err := functions.LoadProjectionSource(rt, c.app, name, src); err != nil {
			return nil, err
		}
	default:
		// effect/http/cron: compile it, and mirror the one other thing
		// LoadDir does for this tier — validate a cron schedule. Compiling
		// alone let a file with an impossible schedule be written, and it
		// then aborted EVERY later reload, which is precisely what refusing
		// unloadable writes exists to prevent.
		if err := rt.RegisterEventFunction([]string{"__check"}, name, src); err != nil {
			return nil, err
		}
		if decl.Cron != "" {
			if err := rt.RegisterCronFunction(decl.Cron, name, src); err != nil {
				return nil, err
			}
		}
	}
	return decl, nil
}

// dryRunRequest is the body of POST /api/cqrs/admin/dryrun.
//
// Mode is explicit and required: the caller has just read the file and knows
// its kind, and a server-side guess would fail by silently running the wrong
// check.
type dryRunRequest struct {
	Name     string          `json:"name"`
	Source   string          `json:"source"`
	Mode     string          `json:"mode"` // decider | decide | projection | compile
	StreamID string          `json:"streamId,omitempty"`
	Command  string          `json:"command,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Diff     bool            `json:"diff,omitempty"`
}

// handleDryRun runs candidate source against real history without appending
// events or touching collections — the HTTP face of the `dryrun` CLI.
func (c *components) handleDryRun(re *core.RequestEvent) error {
	payload, err := io.ReadAll(re.Request.Body)
	if err != nil {
		return apis.NewBadRequestError("failed reading request body", err)
	}
	var req dryRunRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return apis.NewBadRequestError("invalid JSON body: "+err.Error(), err)
	}
	if req.Name == "" {
		req.Name = "candidate.js"
	}

	// candidates are compiled into a scratch runtime, never the live one
	rt := functions.NewGojaRuntime(func(string, ...any) {})
	rt.SetReader(functions.NewAppReader(c.app))
	rt.SetStore(c.store)

	switch req.Mode {
	case "compile":
		decl, err := c.checkFunctionSource(req.Name, req.Source)
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		return re.JSON(http.StatusOK, map[string]any{
			"mode": req.Mode, "ok": true, "declaration": decl,
			"summary": "Parses and compiles. Effect-tier functions have no history to fold, so this is the whole check.",
		})

	case "decider":
		spec, err := functions.LoadDeciderSource(rt, req.Name, req.Source)
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		// Apply the SAME gate a reload would: the contract probe (does it
		// define decide/evolve and its declared transforms?) plus handles
		// coverage. Folding alone answers nothing for an aggregate with no
		// history yet — a decider missing decide() would "pass" vacuously,
		// then be refused at activation. The operator's real question is
		// "will the reload accept this?", so ask it here.
		if err := functions.ValidateDeciderSpec(c.store, spec); err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		res, err := functions.DryRunDecider(c.store, spec, req.StreamID)
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		out := map[string]any{
			"mode": req.Mode, "ok": true, "aggregate": res.Aggregate,
			"streams": res.Streams, "events": res.Events,
			"summary": fmt.Sprintf("Contract and //@handles coverage validated, then folded %d stream(s), %d event(s) of aggregate %q without appending anything. A reload would accept this decider.",
				res.Streams, res.Events, res.Aggregate),
		}
		if req.StreamID != "" {
			out["state"] = res.State
		}
		return re.JSON(http.StatusOK, out)

	case "decide":
		if req.StreamID == "" || req.Command == "" {
			return apis.NewBadRequestError("decide needs streamId and command", nil)
		}
		spec, err := functions.LoadDeciderSource(rt, req.Name, req.Source)
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		cmdPayload := json.RawMessage(`{}`)
		if len(req.Payload) > 0 && json.Valid(req.Payload) {
			cmdPayload = req.Payload
		}
		meta := map[string]any{
			"now":   time.Now().UTC().Format("2006-01-02 15:04:05.000Z"),
			"actor": "dryrun",
		}
		produced, err := functions.DryRunDecide(c.store, spec, req.StreamID,
			decider.Command{Name: req.Command, Payload: cmdPayload}, meta)
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		// "produced", not "events": the other modes report an event COUNT
		// under that name, and one field meaning two shapes by mode is a
		// trap for every client
		return re.JSON(http.StatusOK, map[string]any{
			"mode": req.Mode, "ok": true, "produced": produced,
			"summary": fmt.Sprintf("%q on %s/%s would produce %d event(s). Nothing was appended.",
				req.Command, spec.Aggregate, req.StreamID, len(produced)),
		})

	case "projection":
		spec, err := functions.LoadProjectionSource(rt, c.app, req.Name, req.Source)
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		res, err := functions.DryRunProjection(c.store, spec)
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		rows := 0
		for _, r := range res.Rows {
			rows += len(r)
		}
		summary := fmt.Sprintf("Simulated %q over %d event(s) in memory: %d upsert(s), %d delete(s), %d final row(s) across %d collection(s).",
			res.Name, res.Events, res.Upserts, res.Deletes, rows, len(spec.Schemas))
		if res.IgnoredValues > 0 {
			summary += fmt.Sprintf(" %d returned value(s) were NOT row ops and would be discarded at runtime.",
				res.IgnoredValues)
		}
		out := map[string]any{
			"mode": req.Mode, "ok": true, "name": res.Name,
			"events": res.Events, "upserts": res.Upserts, "deletes": res.Deletes,
			"rows": rows, "collections": len(spec.Schemas),
			"ignoredValues": res.IgnoredValues,
			"summary":       summary,
		}
		if req.Diff {
			diffs, err := c.diffProjection(spec, res)
			if err != nil {
				return apis.NewBadRequestError(err.Error(), err)
			}
			out["diff"] = diffs
		}
		return re.JSON(http.StatusOK, out)

	default:
		return apis.NewBadRequestError(
			fmt.Sprintf("unknown dryrun mode %q: expected decider, decide, projection or compile", req.Mode), nil)
	}
}

// diffProjection compares a simulation against the live collections, per
// collection. Note the M7 caveat: read-modify-write projections read live
// state during simulation, so only absolute-recompute projections are
// expected to come back clean.
func (c *components) diffProjection(spec *functions.ProjectionSpec, res *functions.ProjectionDryRun) (map[string][]string, error) {
	out := map[string][]string{}
	for _, s := range spec.Schemas {
		live, err := c.app.FindRecordsByFilter(s.Collection, "", "", -1, 0)
		if err != nil {
			return nil, err
		}
		liveRows := make([]map[string]any, 0, len(live))
		for _, rec := range live {
			liveRows = append(liveRows, rec.PublicExport())
		}
		out[s.Collection] = functions.DiffRows(res.Rows[s.Collection], liveRows, s.Key)
	}
	return out, nil
}

// listFunctionFiles reports every .js file in dir with what it declares.
// A file that does not parse is listed with its error rather than omitted —
// it is exactly the file an operator needs to find, since it blocks reloads.
func listFunctionFiles(dir string) ([]functionFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []functionFile{}, nil // functions are optional
		}
		return nil, err
	}
	files := make([]functionFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".js") {
			continue
		}
		f := functionFile{Name: entry.Name()}
		if info, err := entry.Info(); err == nil {
			f.Size = info.Size()
			f.Modified = info.ModTime().UTC().Format("2006-01-02 15:04:05.000Z")
		}
		if _, err := os.Stat(filepath.Join(dir, entry.Name()+previousSuffix)); err == nil {
			f.HasPrevious = true
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			f.Error = err.Error()
			files = append(files, f)
			continue
		}
		if d, derr := functions.Declares(entry.Name(), string(raw)); derr == nil {
			f.Declaration = d
		} else {
			f.Error = derr.Error()
		}
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}
