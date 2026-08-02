package functions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dop251/goja"
	"github.com/pocketbase/pocketbase/core"
)

// LoadDir loads functions from dir (PocketBase pb_hooks precedent).
//
// Files ending in .js declare their triggers via directives in the first
// lines:
//
//	//@trigger event TaskCreated TaskCompleted   -> durable event function
//	//@trigger http                              -> served at /api/fn/{basename}
//	//@trigger projection <name> on <EventTypes> -> checkpointed JS projection;
//	//@schema <collection> <field>:<type> ...       requires //@schema + //@key
//	//@key <field>
//
// Event/http files may combine freely; a projection file must be
// projection-only. Files without directives are ignored (logged).
// A missing dir is not an error: functions are optional.
func LoadDir(rt *GojaRuntime, app core.App, dir string) (*HTTPRegistry, []*ProjectionSpec, error) {
	httpReg := NewHTTPRegistry(rt)
	var projs []*ProjectionSpec

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return httpReg, projs, nil
		}
		return nil, nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".js") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, err
		}
		src := string(raw)
		t, err := parseTriggers(src)
		if err != nil {
			return nil, nil, fmt.Errorf("functions: %s: %w", entry.Name(), err)
		}

		if t.empty() {
			rt.logger("function file has no //@trigger directive, ignored", "file", path)
			continue
		}

		if t.projection != "" {
			if len(t.eventTypes) > 0 || t.isHTTP {
				return nil, nil, fmt.Errorf("functions: %s: a projection file must be projection-only", entry.Name())
			}
			spec, err := buildProjectionSpec(rt, app, entry.Name(), src, t)
			if err != nil {
				return nil, nil, err
			}
			projs = append(projs, spec)
			rt.logger("JS projection registered", "name", spec.Name, "collection", spec.Schema.Collection)
			continue
		}

		if len(t.eventTypes) > 0 {
			if err := rt.RegisterEventFunction(t.eventTypes, entry.Name(), src); err != nil {
				return nil, nil, err
			}
		}
		if t.isHTTP {
			prog, err := goja.Compile(entry.Name(), src, false)
			if err != nil {
				return nil, nil, fmt.Errorf("functions: compile %s: %w", entry.Name(), err)
			}
			name := strings.TrimSuffix(entry.Name(), ".js")
			httpReg.register(name, prog)
			rt.logger("HTTP function registered", "name", name, "path", "/api/fn/"+name)
		}
	}

	return httpReg, projs, nil
}

func buildProjectionSpec(rt *GojaRuntime, app core.App, filename, src string, t triggers) (*ProjectionSpec, error) {
	if t.schemaRaw == "" {
		return nil, fmt.Errorf("functions: %s: projection %q is missing its //@schema directive", filename, t.projection)
	}
	schema, err := parseSchemaDirective(t.schemaRaw, t.key)
	if err != nil {
		return nil, fmt.Errorf("functions: %s: %w", filename, err)
	}
	prog, err := goja.Compile(filename, src, false)
	if err != nil {
		return nil, fmt.Errorf("functions: compile %s: %w", filename, err)
	}
	return &ProjectionSpec{
		Name:       t.projection,
		EventTypes: t.projectionOn,
		Schema:     schema,
		Prog:       prog,
		runtime:    rt,
		app:        app,
	}, nil
}

// triggers holds the parsed //@ directives of one function file.
type triggers struct {
	eventTypes   []string // //@trigger event ...
	isHTTP       bool     // //@trigger http
	projection   string   // //@trigger projection <name> on ...
	projectionOn []string
	schemaRaw    string // //@schema ...
	key          string // //@key ...
}

func (t triggers) empty() bool {
	return len(t.eventTypes) == 0 && !t.isHTTP && t.projection == "" && t.schemaRaw == "" && t.key == ""
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
			default:
				return t, fmt.Errorf("unknown //@trigger kind %q", fields[1])
			}
		case "schema":
			t.schemaRaw = strings.TrimSpace(strings.TrimPrefix(rest, "schema"))
		case "key":
			if len(fields) != 2 {
				return t, fmt.Errorf("//@key wants exactly one field name")
			}
			t.key = fields[1]
		}
	}
	return t, nil
}
