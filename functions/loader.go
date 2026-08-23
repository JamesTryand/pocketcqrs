package functions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dop251/goja"
	"github.com/pocketbase/pocketbase/core"
)

// LoadResult bundles everything found in a functions dir.
type LoadResult struct {
	HTTP        *HTTPRegistry
	Projections []*ProjectionSpec
	Deciders    []*DeciderSpec
	Reactors    []*ReactorSpec
}

// Kind names what a function file declares, using the same branch the loader
// takes over it.
type Kind string

const (
	// KindNone is a .js file with no //@trigger directive: the loader
	// ignores it, and a reload cleanly drops whatever it used to declare.
	KindNone Kind = "none"
	// KindEffect covers event, http and cron functions — the tier that
	// reloads in any mode, because it declares no schema.
	KindEffect     Kind = "effect"
	KindProjection Kind = "projection"
	KindDecider    Kind = "decider"
	// KindReactor is a //@trigger reactor file: a durable consumer that maps
	// events to COMMANDS, dispatched through the decider registry. It is
	// effect-tier for reload purposes (it declares no schema) but it is its
	// own kind, because it is the only tier that causes new writes.
	KindReactor Kind = "reactor"
)

// Declaration is what one function file's directives declare.
//
// It exists so that callers which need to classify a file WITHOUT loading it
// — the admin API listing pb_functions/, the editor choosing which dry-run
// to offer — reach the same verdict the loader will, rather than growing a
// parallel classifier that drifts from it. LoadDir branches on this too.
type Declaration struct {
	Kind        Kind     `json:"kind"`
	EventTypes  []string `json:"eventTypes,omitempty"`
	HTTP        bool     `json:"http,omitempty"`
	Cron        string   `json:"cron,omitempty"`
	Projection  string   `json:"projection,omitempty"`
	Collections []string `json:"collections,omitempty"`
	Aggregate   string   `json:"aggregate,omitempty"`
	Handles     []string `json:"handles,omitempty"`
	Commands    []string `json:"commands,omitempty"`
	// Produces maps a command name to the event types it may append. It is
	// the one thing //@commands and //@handles could not say between them:
	// each lists a side, neither joins a pair. Optional and documentation
	// only, like //@commands — decide() still adjudicates.
	Produces   map[string][]string `json:"produces,omitempty"`
	Reactor    []string            `json:"reactor,omitempty"`
	Dispatches []string            `json:"dispatches,omitempty"`
	// SchemaBearing reports whether ACTIVATING this file needs the
	// maintenance barrier: projection schemas and deciders move only in
	// maintenance, effect/http/cron functions move in any mode.
	SchemaBearing bool `json:"schemaBearing"`
}

// Declares parses a function file's directives and reports what it declares.
// name is used for error messages only.
func Declares(name, src string) (*Declaration, error) {
	t, err := parseTriggers(src)
	if err != nil {
		return nil, fmt.Errorf("functions: %s: %w", name, err)
	}
	return declaration(name, t)
}

func declaration(name string, t triggers) (*Declaration, error) {
	d := &Declaration{
		Kind:       KindNone,
		EventTypes: t.eventTypes,
		HTTP:       t.isHTTP,
		Cron:       t.cron,
		Projection: t.projection,
		Aggregate:  t.decider,
		Handles:    t.handles,
		Commands:   t.commands,
		Produces:   t.produces,
		Reactor:    t.reactor,
		Dispatches: t.dispatches,
	}
	if t.empty() {
		return d, nil
	}

	// Single-purpose means single-purpose, cron included. This used to count
	// event/http but not cron, so a file declaring both a cron job and a
	// projection loaded happily with the cron trigger silently discarded —
	// the same class of quiet wrong-doing as a projection returning
	// non-ops. Nothing sensible declares both, and a refusal names the
	// problem where a silent drop leaves a job that simply never runs.
	kinds := 0
	if len(t.eventTypes) > 0 || t.isHTTP || t.cron != "" {
		kinds++
	}
	if t.projection != "" {
		kinds++
	}
	if t.decider != "" {
		kinds++
	}
	// reactor gets its OWN bucket rather than joining event/http/cron. A file
	// declaring both //@trigger event X and //@trigger reactor X is two
	// delivery paths over one event, with two checkpoints, and there is no
	// sensible reading of what it means — while a silent merge would give
	// one of them no way to be noticed. Same call as the cron+projection
	// quirk, made before it bites rather than after.
	if len(t.reactor) > 0 {
		kinds++
	}
	if kinds > 1 {
		return nil, fmt.Errorf("functions: %s: projection, decider and reactor files must be single-purpose", name)
	}
	if len(t.dispatches) > 0 && len(t.reactor) == 0 {
		return nil, fmt.Errorf("functions: %s: //@dispatches requires //@trigger reactor", name)
	}
	if len(t.produces) > 0 {
		if t.decider == "" {
			return nil, fmt.Errorf("functions: %s: //@produces requires //@trigger decider", name)
		}
		// A produced event that is not in //@handles cannot be folded back,
		// so the decider could append something it can never replay. That is
		// a contradiction inside one file, and refusing it here is cheaper
		// than discovering it when a stream fails to load.
		declared := map[string]bool{}
		for _, h := range t.handles {
			declared[h] = true
		}
		known := map[string]bool{}
		for _, c := range t.commands {
			known[c] = true
		}
		for cmd, evs := range t.produces {
			if len(t.commands) > 0 && !known[cmd] {
				return nil, fmt.Errorf("functions: %s: //@produces names command %q, which is not in //@commands", name, cmd)
			}
			for _, ev := range evs {
				if !declared[ev] {
					return nil, fmt.Errorf("functions: %s: //@produces says %q appends %q, which is not in //@handles",
						name, cmd, ev)
				}
			}
		}
	}

	// Kind mirrors the branch LoadDir takes: projection, decider and
	// reactor files are handled by name, everything else falls to the
	// effect tier.
	switch {
	case t.projection != "":
		d.Kind = KindProjection
	case t.decider != "":
		d.Kind = KindDecider
	case len(t.reactor) > 0:
		d.Kind = KindReactor
	default:
		d.Kind = KindEffect
	}

	// A malformed //@schema is reported, not skipped: the listing's whole
	// job is to surface the file that will block a reload, and a projection
	// shown as healthy-with-no-collections would hide exactly that.
	for _, rs := range t.schemas {
		s, err := parseSchemaDirective(rs.raw, rs.key)
		if err != nil {
			return nil, fmt.Errorf("functions: %s: %w", name, err)
		}
		d.Collections = append(d.Collections, s.Collection)
	}
	d.SchemaBearing = d.Kind == KindProjection || d.Kind == KindDecider
	return d, nil
}

// LoadDir loads functions from dir (PocketBase pb_hooks precedent).
//
// Files ending in .js declare their triggers via directives in the first
// lines:
//
//	//@trigger event TaskCreated TaskCompleted   -> durable event function
//	//@trigger http                              -> served at /api/fn/{basename}
//	//@trigger projection <name> on <EventTypes> -> checkpointed JS projection;
//	//@schema <collection> <field>:<type> ...       requires //@schema + //@key;
//	//@key <field>                                  type: text|number|bool|date|json
//	                                                or relation(<collection>).
//	                                                Repeat //@schema (+ its //@key)
//	                                                to write multiple collections;
//	                                                ops then need a "collection".
//	                                                Each //@key pairs with the most
//	                                                recent //@schema.
//	//@rule <collection> <value>                  -> optional: overrides
//	                                                --cqrsSchemaDefaultRule for
//	                                                this collection's ListRule/
//	                                                ViewRule. value is "public",
//	                                                "authenticated", or a raw
//	                                                PocketBase rule expression.
//	//@trigger reactor <EventTypes...>             -> JS reactor (tier 4): maps
//	//@dispatches <aggregate>/<Command>...          events to COMMANDS through
//	                                                the decider registry.
//	                                                reactTo(event) RETURNS
//	                                                {aggregate,id,command,payload}
//	                                                descriptors; //@dispatches
//	                                                declares them for the
//	                                                catalog, since a command
//	                                                leaves no trace until it
//	                                                has actually fired.
//	//@trigger decider <aggregate>               -> JS decider (tier 3);
//	//@handles <EventTypes...>                      requires //@handles
//	//@commands <Names...>                       -> optional: the commands this
//	                                                decider accepts. Documentation
//	                                                only — commands leave no trace
//	                                                in the log, so nothing else can
//	                                                report them.
//	//@transform <Type> <from> <to>              -> upcaster fn transform_<Type>_<from>_to_<to>
//
// Event/http files may combine freely; projection and decider files must
// be single-purpose. Files without directives are ignored (logged).
// A missing dir is not an error: functions are optional.
func LoadDir(rt *GojaRuntime, app core.App, dir string) (*LoadResult, error) {
	result := &LoadResult{HTTP: NewHTTPRegistry(rt)}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".js") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		src := string(raw)
		t, err := parseTriggers(src)
		if err != nil {
			return nil, fmt.Errorf("functions: %s: %w", entry.Name(), err)
		}

		// classify exactly as Declares does — the admin API and the editor
		// branch on the same verdict
		d, err := declaration(entry.Name(), t)
		if err != nil {
			return nil, err
		}
		if d.Kind == KindNone {
			rt.logger("function file has no //@trigger directive, ignored", "file", path)
			continue
		}

		switch {
		case t.projection != "":
			spec, err := buildProjectionSpec(rt, app, entry.Name(), src, t)
			if err != nil {
				return nil, err
			}
			result.Projections = append(result.Projections, spec)
			rt.logger("JS projection registered", "name", spec.Name, "collections", spec.Collections())

		case t.decider != "":
			spec, err := buildDeciderSpec(rt, entry.Name(), src, t)
			if err != nil {
				return nil, err
			}
			result.Deciders = append(result.Deciders, spec)
			rt.logger("JS decider registered", "aggregate", spec.Aggregate)

		case len(t.reactor) > 0:
			spec, err := buildReactorSpec(rt, entry.Name(), src, t)
			if err != nil {
				return nil, err
			}
			result.Reactors = append(result.Reactors, spec)
			rt.logger("JS reactor registered", "reactor", spec.Reactor, "on", spec.EventTypes)

		default:
			if len(t.eventTypes) > 0 {
				if err := rt.RegisterEventFunction(t.eventTypes, entry.Name(), src); err != nil {
					return nil, err
				}
			}
			if t.cron != "" {
				if err := rt.RegisterCronFunction(t.cron, entry.Name(), src); err != nil {
					return nil, err
				}
				rt.logger("cron function registered", "name", entry.Name(), "schedule", t.cron)
			}
			if t.isHTTP {
				prog, err := goja.Compile(entry.Name(), src, false)
				if err != nil {
					return nil, fmt.Errorf("functions: compile %s: %w", entry.Name(), err)
				}
				name := strings.TrimSuffix(entry.Name(), ".js")
				result.HTTP.register(name, prog)
				rt.logger("HTTP function registered", "name", name, "path", "/api/fn/"+name)
			}
		}
	}

	return result, nil
}

func buildProjectionSpec(rt *GojaRuntime, app core.App, filename, src string, t triggers) (*ProjectionSpec, error) {
	if len(t.schemas) == 0 {
		return nil, fmt.Errorf("functions: %s: projection %q is missing its //@schema directive", filename, t.projection)
	}
	schemas := make([]*SchemaSpec, 0, len(t.schemas))
	seen := map[string]bool{}
	remainingRules := make(map[string]string, len(t.rules))
	for k, v := range t.rules {
		remainingRules[k] = v
	}
	for _, rs := range t.schemas {
		s, err := parseSchemaDirective(rs.raw, rs.key)
		if err != nil {
			return nil, fmt.Errorf("functions: %s: %w", filename, err)
		}
		if seen[s.Collection] {
			return nil, fmt.Errorf("functions: %s: duplicate //@schema for collection %q", filename, s.Collection)
		}
		seen[s.Collection] = true
		if value, ok := remainingRules[s.Collection]; ok {
			s.RuleOverride = &value
			delete(remainingRules, s.Collection)
		}
		schemas = append(schemas, s)
	}
	// A //@rule left unmatched names a collection this file never declared
	// with //@schema — refused rather than silently ignored, the same
	// posture //@key already takes for an unpaired directive.
	for collection := range remainingRules {
		return nil, fmt.Errorf("functions: %s: //@rule names collection %q, which has no //@schema in this file", filename, collection)
	}
	prog, err := goja.Compile(filename, src, false)
	if err != nil {
		return nil, fmt.Errorf("functions: compile %s: %w", filename, err)
	}
	return &ProjectionSpec{
		Name:       t.projection,
		EventTypes: t.projectionOn,
		Schemas:    schemas,
		Prog:       prog,
		runtime:    rt,
		app:        app,
	}, nil
}

func buildDeciderSpec(rt *GojaRuntime, filename, src string, t triggers) (*DeciderSpec, error) {
	if len(t.handles) == 0 {
		return nil, fmt.Errorf("functions: %s: decider %q is missing its //@handles directive", filename, t.decider)
	}
	if !validName.MatchString(t.decider) {
		return nil, fmt.Errorf("functions: %s: invalid aggregate name %q", filename, t.decider)
	}
	prog, err := goja.Compile(filename, src, false)
	if err != nil {
		return nil, fmt.Errorf("functions: compile %s: %w", filename, err)
	}
	return &DeciderSpec{
		Aggregate:  t.decider,
		Handles:    t.handles,
		Commands:   t.commands,
		Produces:   t.produces,
		Transforms: t.transforms,
		Prog:       prog,
		runtime:    rt,
	}, nil
}

// rawSchema is one //@schema directive and the //@key paired with it
// (a //@key belongs to the most recent //@schema).
type rawSchema struct {
	raw string
	key string
}

// triggers holds the parsed //@ directives of one function file.
type triggers struct {
	eventTypes   []string // //@trigger event ...
	isHTTP       bool     // //@trigger http
	cron         string   // //@trigger cron <schedule>
	projection   string   // //@trigger projection <name> on ...
	projectionOn []string
	schemas      []rawSchema         // //@schema ... (+ its //@key), repeatable
	rules        map[string]string   // //@rule <collection> <value> (optional; keyed by collection)
	decider      string              // //@trigger decider <aggregate>
	handles      []string            // //@handles ...
	commands     []string            // //@commands ... (optional; documentation)
	produces     map[string][]string // //@produces <Command> <Event...> (optional)
	reactor      []string            // //@trigger reactor <EventTypes...>
	dispatches   []string            // //@dispatches <aggregate>/<Command>... (optional)
	transforms   []TransformSpec
}

func (t triggers) empty() bool {
	return len(t.eventTypes) == 0 && !t.isHTTP && t.cron == "" && t.projection == "" && len(t.schemas) == 0 &&
		t.decider == "" && len(t.handles) == 0 && len(t.commands) == 0 && len(t.transforms) == 0 &&
		len(t.reactor) == 0 && len(t.dispatches) == 0 && len(t.produces) == 0 && len(t.rules) == 0
}

// parseTriggers scans the leading comment lines for //@ directives.
func parseTriggers(src string) (triggers, error) {
	var t triggers
	for line := range strings.Lines(src) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "//") {
			break // directives must lead the file
		}
		rest, ok := strings.CutPrefix(line, "//@")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "trigger":
			if len(fields) < 2 {
				return t, fmt.Errorf("empty //@trigger directive")
			}
			switch fields[1] {
			case "event":
				t.eventTypes = append(t.eventTypes, fields[2:]...)
			case "http":
				t.isHTTP = true
			case "projection":
				if len(fields) < 4 || fields[2] == "" || fields[3] != "on" || len(fields) < 5 {
					return t, fmt.Errorf("//@trigger projection wants: projection <name> on <EventTypes...>")
				}
				t.projection = fields[2]
				t.projectionOn = fields[4:]
			case "decider":
				if len(fields) != 3 {
					return t, fmt.Errorf("//@trigger decider wants: decider <aggregate>")
				}
				t.decider = fields[2]
			case "cron":
				if len(fields) < 3 {
					return t, fmt.Errorf("//@trigger cron wants: cron <schedule>")
				}
				if t.cron != "" {
					return t, fmt.Errorf("only one //@trigger cron per file")
				}
				t.cron = strings.Join(fields[2:], " ")
			case "reactor":
				if len(fields) < 3 {
					return t, fmt.Errorf("//@trigger reactor wants: reactor <EventTypes...>")
				}
				t.reactor = append(t.reactor, fields[2:]...)
			default:
				return t, fmt.Errorf("unknown //@trigger kind %q", fields[1])
			}
		case "schema":
			t.schemas = append(t.schemas, rawSchema{
				raw: strings.TrimSpace(strings.TrimPrefix(rest, "schema")),
			})
		case "key":
			if len(fields) != 2 {
				return t, fmt.Errorf("//@key wants exactly one field name")
			}
			if len(t.schemas) == 0 {
				return t, fmt.Errorf("//@key requires a preceding //@schema")
			}
			last := &t.schemas[len(t.schemas)-1]
			if last.key != "" {
				return t, fmt.Errorf("duplicate //@key for one //@schema")
			}
			last.key = fields[1]
		case "rule":
			// //@rule <collection> <public|authenticated|rule-expression> —
			// Item 9's per-collection override of --cqrsSchemaDefaultRule.
			// Named by collection rather than paired positionally like
			// //@key, since a value may itself contain spaces (a raw rule
			// expression) and a file may declare //@rule before or after
			// the //@schema it targets.
			raw := strings.TrimSpace(strings.TrimPrefix(rest, "rule"))
			collection, value, ok := strings.Cut(raw, " ")
			value = strings.TrimSpace(value)
			if !ok || collection == "" || value == "" {
				return t, fmt.Errorf("//@rule wants: rule <collection> <public|authenticated|rule-expression>")
			}
			if !validName.MatchString(collection) {
				return t, fmt.Errorf("invalid collection name %q in //@rule", collection)
			}
			if t.rules == nil {
				t.rules = map[string]string{}
			}
			if _, dup := t.rules[collection]; dup {
				return t, fmt.Errorf("duplicate //@rule for collection %q", collection)
			}
			t.rules[collection] = value
		case "handles":
			t.handles = append(t.handles, fields[1:]...)
		case "commands":
			t.commands = append(t.commands, fields[1:]...)
		case "produces":
			// //@produces <Command> <Event...> — the association //@commands
			// and //@handles could not express between them. Each names the
			// commands or the events; nothing joined a pair, so an export
			// had to give every slice its aggregate's whole event set.
			if len(fields) < 3 {
				return t, fmt.Errorf("//@produces wants: produces <Command> <EventType...>")
			}
			if t.produces == nil {
				t.produces = map[string][]string{}
			}
			cmd := fields[1]
			if _, seen := t.produces[cmd]; seen {
				return t, fmt.Errorf("//@produces declared twice for command %q; list its events on one line", cmd)
			}
			t.produces[cmd] = append([]string(nil), fields[2:]...)
		case "dispatches":
			if len(fields) < 2 {
				return t, fmt.Errorf("//@dispatches wants: dispatches <aggregate>/<Command>...")
			}
			// A malformed entry is REFUSED, not dropped. Same reasoning as
			// //@schema: this list is the only declared record of what a
			// reactor sends (the resulting event proves it only once it has
			// actually fired), so silently discarding a typo would leave the
			// catalog confidently reporting less than the truth.
			for _, d := range fields[1:] {
				agg, cmd, ok := strings.Cut(d, "/")
				if !ok || agg == "" || cmd == "" {
					return t, fmt.Errorf("//@dispatches entry %q must be <aggregate>/<Command>", d)
				}
			}
			t.dispatches = append(t.dispatches, fields[1:]...)
		case "transform":
			if len(fields) != 4 {
				return t, fmt.Errorf("//@transform wants: transform <Type> <from> <to>")
			}
			from, err := parseVersion(fields[2])
			if err != nil {
				return t, fmt.Errorf("//@transform from-version: %w", err)
			}
			to, err := parseVersion(fields[3])
			if err != nil {
				return t, fmt.Errorf("//@transform to-version: %w", err)
			}
			t.transforms = append(t.transforms, TransformSpec{Type: fields[1], From: from, To: to})
		}
	}
	return t, nil
}

func parseVersion(s string) (int64, error) {
	var v int64
	if _, err := fmt.Sscan(s, &v); err != nil || v <= 0 {
		return 0, fmt.Errorf("invalid version %q", s)
	}
	return v, nil
}
