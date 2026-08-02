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
//	//@trigger decider <aggregate>               -> JS decider (tier 3);
//	//@handles <EventTypes...>                      requires //@handles
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

		if t.empty() {
			rt.logger("function file has no //@trigger directive, ignored", "file", path)
			continue
		}

		kinds := 0
		if len(t.eventTypes) > 0 || t.isHTTP {
			kinds++
		}
		if t.projection != "" {
			kinds++
		}
		if t.decider != "" {
			kinds++
		}
		if kinds > 1 {
			return nil, fmt.Errorf("functions: %s: projection and decider files must be single-purpose", entry.Name())
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
	for _, rs := range t.schemas {
		s, err := parseSchemaDirective(rs.raw, rs.key)
		if err != nil {
			return nil, fmt.Errorf("functions: %s: %w", filename, err)
		}
		if seen[s.Collection] {
			return nil, fmt.Errorf("functions: %s: duplicate //@schema for collection %q", filename, s.Collection)
		}
		seen[s.Collection] = true
		schemas = append(schemas, s)
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
	schemas      []rawSchema // //@schema ... (+ its //@key), repeatable
	decider      string      // //@trigger decider <aggregate>
	handles      []string    // //@handles ...
	transforms   []TransformSpec
}

func (t triggers) empty() bool {
	return len(t.eventTypes) == 0 && !t.isHTTP && t.cron == "" && t.projection == "" && len(t.schemas) == 0 &&
		t.decider == "" && len(t.handles) == 0 && len(t.transforms) == 0
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
		case "handles":
			t.handles = append(t.handles, fields[1:]...)
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
