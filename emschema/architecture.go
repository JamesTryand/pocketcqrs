package emschema

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/jamestryand/pocketcqrs/edges"
	"github.com/jamestryand/pocketcqrs/scaffold"
)

// Architecture is the part of a coordinating document (eventmodelschema's
// architecture.schema.json, 0.2.0) a system's generation reads: its systems
// and its internal edges. Its full check (every rule, every system's
// document) is eventmodelschema's architecture-check.js; this reads only what
// one system's edges need, and refuses what it cannot carry.
type Architecture struct {
	Version string                        `json:"architectureSchemaVersion"`
	ID      string                        `json:"id"`
	Systems map[string]ArchitectureSystem `json:"systems"`
	Edges   []ArchitectureEdge            `json:"edges"`
	// dir is the coordinating document's directory: system paths are
	// relative to it.
	dir string
}

// ArchitectureSystem is one system: a document by path, or a placeholder.
type ArchitectureSystem struct {
	Name     string `json:"name"`
	Document *struct {
		Path string `json:"path"`
	} `json:"document,omitempty"`
}

// ArchitectureEdge is one edge. Only internal edges are read.
type ArchitectureEdge struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	From *struct {
		System  string `json:"system"`
		Event   string `json:"event"`
		Version int    `json:"version"`
	} `json:"from,omitempty"`
	To *struct {
		System  string `json:"system"`
		Command string `json:"command"`
	} `json:"to,omitempty"`
	TargetID json.RawMessage   `json:"targetId,omitempty"`
	Fields   map[string]string `json:"fields,omitempty"`
	Examples []EdgeExample     `json:"examples,omitempty"`
}

// EdgeExample is an edge's test: given the source event's data, then the
// command (target id and fields, exactly).
type EdgeExample struct {
	Name  string         `json:"name"`
	Given map[string]any `json:"given"`
	Then  struct {
		TargetID string         `json:"targetId"`
		Fields   map[string]any `json:"fields"`
	} `json:"then"`
}

// SystemEdges is what one system's generation gets from a coordinating
// document: the edges into it, the events it publishes for edges out of it,
// and how its edges' examples went.
type SystemEdges struct {
	Architecture string
	System       string
	In           []edges.In
	Out          []edges.Out
	// Examples names each example run, and whether it held.
	Examples []EdgeExampleResult
}

// EdgeExampleResult is one example's outcome.
type EdgeExampleResult struct {
	Edge, Name string
	Passed     bool
	Detail     string
}

// LoadArchitecture reads a coordinating document.
func LoadArchitecture(path string) (*Architecture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var a Architecture
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if a.Version == "" || len(a.Systems) == 0 {
		return nil, fmt.Errorf("%s is not a coordinating document (no architectureSchemaVersion or systems)", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	a.dir = filepath.Dir(abs)
	return &a, nil
}

// EdgesFor reads system's edges from a, against doc (the system's own
// document, found at docPath). Everything it cannot carry is refused, naming
// the edge: an error. The examples are run; a failing one is an error too,
// since the translation they describe is generated, not hand-written.
func EdgesFor(a *Architecture, system string, doc *Document, docPath string, opts Options) (*SystemEdges, error) {
	sys, ok := a.Systems[system]
	if !ok {
		return nil, fmt.Errorf("the coordinating document %q has no system %q", a.ID, system)
	}
	if sys.Document == nil {
		return nil, fmt.Errorf("system %q is a placeholder in %q, so it has no document to generate", system, a.ID)
	}
	want, err := filepath.Abs(filepath.Join(a.dir, sys.Document.Path))
	if err != nil {
		return nil, err
	}
	got, err := filepath.Abs(docPath)
	if err != nil {
		return nil, err
	}
	if !samePath(want, got) {
		return nil, fmt.Errorf("system %q's document in %q is %s, not %s", system, a.ID, want, got)
	}

	se := &SystemEdges{Architecture: a.ID, System: system}
	var problems []string
	refuse := func(edge, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("edge %q: ", edge)+fmt.Sprintf(format, args...))
	}

	// the events this system publishes: each public event an internal edge
	// starts from, with only the fields some edge from it maps
	type key struct {
		event   string
		version int
	}
	outs := map[key]map[string]bool{}
	for _, e := range a.Edges {
		if e.Kind != "internal" || e.From == nil || e.From.System != system {
			continue
		}
		ev, ok := doc.Events[e.From.Event]
		if !ok {
			refuse(e.ID, "this system has no event %q", e.From.Event)
			continue
		}
		if ev.Public == nil || ev.Public.Version != e.From.Version {
			refuse(e.ID, "event %q is not public at version %d here", e.From.Event, e.From.Version)
			continue
		}
		k := key{e.From.Event, e.From.Version}
		if outs[k] == nil {
			outs[k] = map[string]bool{}
		}
		for _, f := range publishedBy(e) {
			field, ok := fieldOf(ev.Fields, f)
			if !ok {
				refuse(e.ID, "event %q has no field %q", e.From.Event, f)
				continue
			}
			if field.PII {
				refuse(e.ID, "%q is personal data: carrying a personal field over an edge is not built", f)
				continue
			}
			outs[k][f] = true
		}
	}
	for k, fields := range outs {
		ev := doc.Events[k.event]
		o := edges.Out{EventType: TypeName(ev.Name, k.event), System: system, Event: k.event, Version: k.version}
		for f := range fields {
			o.Fields = append(o.Fields, f)
		}
		sort.Strings(o.Fields)
		se.Out = append(se.Out, o)
	}
	sort.Slice(se.Out, func(i, j int) bool { return se.Out[i].Event < se.Out[j].Event })

	// the edges into this system
	for _, e := range a.Edges {
		if e.Kind != "internal" || e.To == nil || e.To.System != system || e.From == nil {
			continue
		}
		cmd, ok := doc.Commands[e.To.Command]
		if !ok {
			refuse(e.ID, "this system has no command %q", e.To.Command)
			continue
		}
		if cmd.Public == nil {
			refuse(e.ID, "command %q is not public here", e.To.Command)
			continue
		}
		target, err := singleTarget(e.TargetID)
		if err != nil {
			refuse(e.ID, "%v", err)
			continue
		}
		agg := cmd.Aggregate
		if agg == "" {
			agg = opts.AggregateOverrides[e.To.Command]
		}
		if agg == "" {
			refuse(e.ID, "command %q has no aggregate (tag it, or --aggregate %s=<name>)", e.To.Command, e.To.Command)
			continue
		}
		for cf := range e.Fields {
			if _, ok := fieldOf(cmd.Fields, cf); !ok {
				refuse(e.ID, "command %q has no field %q", e.To.Command, cf)
			}
		}
		in := edges.In{ID: e.ID, FromSystem: e.From.System, FromEvent: e.From.Event, FromVersion: e.From.Version,
			Aggregate: LowerFirst(scaffold.SanitizeName(agg)), Command: TypeName(cmd.Name, e.To.Command),
			TargetField: target, Fields: e.Fields}
		se.In = append(se.In, in)

		// the examples: what the sender publishes, translated here
		sent := publishedFields(a, e.From.System, e.From.Event, e.From.Version)
		for _, ex := range e.Examples {
			r := EdgeExampleResult{Edge: e.ID, Name: ex.Name}
			m := edges.Message{ID: "example", System: e.From.System, Event: e.From.Event, Version: e.From.Version,
				Data: edges.Only(ex.Given, sent)}
			id, fields, err := in.TranslateFields(m)
			switch {
			case err != nil:
				r.Detail = err.Error()
			case id != ex.Then.TargetID:
				r.Detail = fmt.Sprintf("the target id is %q, not %q", id, ex.Then.TargetID)
			case !sameJSON(fields, ex.Then.Fields):
				r.Detail = fmt.Sprintf("the command's fields are %s, not %s", jsonOf(fields), jsonOf(ex.Then.Fields))
			default:
				r.Passed, r.Detail = true, fmt.Sprintf("%s to %s %s", in.Command, in.Aggregate, id)
			}
			if !r.Passed {
				refuse(e.ID, "example %q: %s", ex.Name, r.Detail)
			}
			se.Examples = append(se.Examples, r)
		}
	}
	sort.Slice(se.In, func(i, j int) bool { return se.In[i].ID < se.In[j].ID })
	if len(problems) > 0 {
		return se, fmt.Errorf("the coordinating document's edges for %q cannot be generated:\n  %s",
			system, strings.Join(problems, "\n  "))
	}
	return se, nil
}

// publishedBy is the source event fields one edge needs: its mapped fields
// and its target field.
func publishedBy(e ArchitectureEdge) []string {
	var out []string
	for _, f := range e.Fields {
		out = append(out, f)
	}
	if t, err := singleTarget(e.TargetID); err == nil {
		out = append(out, t)
	}
	return out
}

// publishedFields is what a sender publishes of one event: the fields some
// internal edge from it, at that version, needs (architecture-check.js's
// published()).
func publishedFields(a *Architecture, system, event string, version int) []string {
	set := map[string]bool{}
	for _, e := range a.Edges {
		if e.Kind != "internal" || e.From == nil || e.From.System != system || e.From.Event != event ||
			e.From.Version != version {
			continue
		}
		for _, f := range publishedBy(e) {
			set[f] = true
		}
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func singleTarget(raw json.RawMessage) (string, error) {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil && one != "" {
		return one, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil && len(many) == 1 {
		return many[0], nil
	}
	return "", fmt.Errorf("a target id made of several fields is not translated yet: name one field")
}

func fieldOf(fields []Field, name string) (Field, bool) {
	for _, f := range fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func sameJSON(a, b map[string]any) bool {
	var x, y any
	_ = json.Unmarshal([]byte(jsonOf(a)), &x)
	_ = json.Unmarshal([]byte(jsonOf(b)), &y)
	return reflect.DeepEqual(x, y)
}

func jsonOf(v map[string]any) string {
	if v == nil {
		v = map[string]any{}
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}
