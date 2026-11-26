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

// ArchitectureSystem is one system: a document by path, a released document
// by its id and tag (0.3.0), or a placeholder.
type ArchitectureSystem struct {
	Name     string `json:"name"`
	Document *struct {
		Path string `json:"path"`
	} `json:"document,omitempty"`
	Release *struct {
		System string `json:"system"`
		Tag    string `json:"tag"`
	} `json:"release,omitempty"`
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
	switch {
	case sys.Release != nil:
		// named by release: the document is the one whose id the release
		// names; where its repository is stays out of the view
		if sys.Release.System != doc.ID {
			return nil, fmt.Errorf("system %q in %q is the release of %q, not this document (%q)",
				system, a.ID, sys.Release.System, doc.ID)
		}
	case sys.Document != nil:
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
	default:
		return nil, fmt.Errorf("system %q is a placeholder in %q, so it has no document to generate", system, a.ID)
	}

	// A message names the system that sent it by that system's document id,
	// never by the name a view gives it: the sender knows its id from its own
	// document, and every view naming it agrees on it.
	se := &SystemEdges{Architecture: a.ID, System: doc.ID}
	var problems []string
	refuse := func(edge, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("edge %q: ", edge)+fmt.Sprintf(format, args...))
	}
	senders := map[string]string{} // a view's name for a sender -> its document id

	// the events this system publishes: each public event an internal edge
	// starts from. When its marker lists the fields it publishes (schema
	// 6.2.0), those, and an edge may read only those: the system's own
	// document says what leaves it, so a link in another document can rely
	// on them. Otherwise only the fields some edge from it here maps.
	type key struct {
		event   string
		version int
	}
	outs := map[key]map[string]bool{}
	publish := func(edgeID, event string, ev Event, f string, k key) {
		field, ok := fieldOf(ev.Fields, f)
		if !ok {
			refuse(edgeID, "event %q has no field %q", event, f)
			return
		}
		if field.PII {
			refuse(edgeID, "%q is personal data: carrying a personal field over an edge is not built", f)
			return
		}
		outs[k][f] = true
	}
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
		listed := ev.Public.Fields
		if outs[k] == nil {
			outs[k] = map[string]bool{}
			for _, f := range listed {
				publish(e.ID, e.From.Event, ev, f, k)
			}
		}
		for _, f := range publishedBy(e) {
			if listed == nil {
				publish(e.ID, e.From.Event, ev, f, k)
			} else if !contains(listed, f) {
				refuse(e.ID, "event %q does not publish %q: its public marker lists only %s", e.From.Event, f,
					strings.Join(listed, ", "))
			}
		}
	}
	// a public event whose marker lists its fields is published whether or not
	// an edge here starts from it: a link in another view may read it, and the
	// system's own document says what leaves it
	for id, ev := range doc.Events {
		if ev.Public == nil || ev.Public.Fields == nil {
			continue
		}
		k := key{id, ev.Public.Version}
		if outs[k] != nil {
			continue
		}
		outs[k] = map[string]bool{}
		for _, f := range ev.Public.Fields {
			field, ok := fieldOf(ev.Fields, f)
			switch {
			case !ok:
				problems = append(problems, fmt.Sprintf("event %q publishes %q, which is not one of its fields", id, f))
			case field.PII:
				problems = append(problems, fmt.Sprintf("event %q: %q is personal data: carrying a personal field "+
					"over an edge is not built", id, f))
			default:
				outs[k][f] = true
			}
		}
	}
	for k, fields := range outs {
		ev := doc.Events[k.event]
		o := edges.Out{EventType: TypeName(ev.Name, k.event), System: doc.ID, Event: k.event, Version: k.version}
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
		from, ok := senders[e.From.System]
		if !ok {
			id, err := a.documentID(e.From.System)
			if err != nil {
				refuse(e.ID, "%v", err)
				continue
			}
			from, senders[e.From.System] = id, id
		}
		in := edges.In{ID: e.ID, FromSystem: from, FromEvent: e.From.Event, FromVersion: e.From.Version,
			Aggregate: LowerFirst(scaffold.SanitizeName(agg)), Command: TypeName(cmd.Name, e.To.Command),
			TargetField: target, Fields: e.Fields}
		se.In = append(se.In, in)

		// the examples: what the sender publishes, translated here
		sent := publishedFields(a, e.From.System, e.From.Event, e.From.Version)
		for _, ex := range e.Examples {
			r := EdgeExampleResult{Edge: e.ID, Name: ex.Name}
			m := edges.Message{ID: "example", System: from, Event: e.From.Event, Version: e.From.Version,
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

// documentID is the document id of the system a names key: the name its
// messages carry. A document by path is read for its id; a release names it.
// A placeholder has no document yet, so no id its messages would carry.
func (a *Architecture) documentID(key string) (string, error) {
	sys, ok := a.Systems[key]
	switch {
	case !ok:
		return "", fmt.Errorf("the coordinating document %q has no system %q", a.ID, key)
	case sys.Release != nil:
		return sys.Release.System, nil
	case sys.Document != nil:
		doc, err := Load(filepath.Join(a.dir, sys.Document.Path))
		if err != nil {
			return "", fmt.Errorf("the sender %q's document: %w", key, err)
		}
		return doc.ID, nil
	default:
		return "", fmt.Errorf("the sender %q is a placeholder: a message names its sender by the sender's "+
			"document id, and a placeholder has none yet", key)
	}
}

// PublishedEdges is what a sender publishes from its own document alone, with
// no coordinating document: each public event whose marker lists its fields
// (schema 6.2.0), with exactly those, under the document's id. An event whose
// marker lists nothing publishes the fields some edge maps, and with no
// coordinating document there is none, so it publishes nothing. nil when
// nothing is published.
func PublishedEdges(doc *Document) (*SystemEdges, error) {
	se := &SystemEdges{System: doc.ID}
	var problems []string
	ids := make([]string, 0, len(doc.Events))
	for id := range doc.Events {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ev := doc.Events[id]
		if ev.Public == nil || ev.Public.Fields == nil {
			continue
		}
		o := edges.Out{EventType: TypeName(ev.Name, id), System: doc.ID, Event: id, Version: ev.Public.Version}
		for _, f := range ev.Public.Fields {
			field, ok := fieldOf(ev.Fields, f)
			switch {
			case !ok:
				problems = append(problems, fmt.Sprintf("event %q publishes %q, which is not one of its fields", id, f))
			case field.PII:
				problems = append(problems, fmt.Sprintf("event %q: %q is personal data: carrying a personal field "+
					"over an edge is not built", id, f))
			default:
				o.Fields = append(o.Fields, f)
			}
		}
		sort.Strings(o.Fields)
		se.Out = append(se.Out, o)
	}
	if len(problems) > 0 {
		return se, fmt.Errorf("what %q publishes cannot be generated:\n  %s", doc.ID, strings.Join(problems, "\n  "))
	}
	if len(se.Out) == 0 {
		return nil, nil
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

// publishedFields is what a sender publishes of one event as far as this
// document's edges say: the fields some internal edge from it, at that
// version, needs (architecture-check.js's published() without a marker
// list). The receiving side's examples use it; a sender whose marker lists
// more publishes more, which changes no translation, since an edge reads
// only what it maps.
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

// EdgesForViews reads one system's edges from several coordinating documents
// (views) at once: a system may be linked in more than one, each held by
// whoever owns those links. In each view the system is the one named
// systems[i] when given (one name for every view, or one per view), else the
// one that is this document (named by its release's id, or by its path). Each
// view is read as EdgesFor reads it, and the results are joined: the edges in,
// the events published (their fields together) and the examples. Views may
// overlap but not contradict: an edge id is one link in every view that holds
// it, the same ends and the same translation, as eventmodelschema's checker
// requires; a contradiction is refused, naming the edge and both views.
func EdgesForViews(views []*Architecture, systems []string, doc *Document, docPath string, opts Options) (*SystemEdges, error) {
	if len(views) == 0 {
		return nil, fmt.Errorf("no coordinating document given")
	}
	if len(systems) > 1 && len(systems) != len(views) {
		return nil, fmt.Errorf("%d system names for %d coordinating documents: give one for all, or one per document",
			len(systems), len(views))
	}
	joined := &SystemEdges{System: doc.ID}
	var ids []string
	inFrom := map[string]string{} // edge id -> the view it was read from
	ins := map[string]edges.In{}
	type key struct {
		event   string
		version int
	}
	outs := map[key]*edges.Out{}
	var problems []string
	for i, a := range views {
		var system string
		switch {
		case len(systems) == 1:
			system = systems[0]
		case len(systems) > 1:
			system = systems[i]
		default:
			var err error
			if system, err = a.systemOf(doc, docPath); err != nil {
				return nil, err
			}
		}
		se, err := EdgesFor(a, system, doc, docPath, opts)
		if err != nil {
			return se, err
		}
		ids = append(ids, a.ID)
		for _, in := range se.In {
			if prev, ok := ins[in.ID]; ok {
				if !reflect.DeepEqual(prev, in) {
					problems = append(problems, fmt.Sprintf("edge %q is one link in %q and another in %q: "+
						"an edge id is the same link in every view", in.ID, inFrom[in.ID], a.ID))
				}
				continue
			}
			ins[in.ID], inFrom[in.ID] = in, a.ID
			joined.In = append(joined.In, in)
		}
		for _, o := range se.Out {
			k := key{o.Event, o.Version}
			if prev, ok := outs[k]; ok {
				for _, f := range o.Fields {
					if !contains(prev.Fields, f) {
						prev.Fields = append(prev.Fields, f)
					}
				}
				sort.Strings(prev.Fields)
				continue
			}
			c := o
			c.Fields = append([]string(nil), o.Fields...)
			outs[k] = &c
		}
		joined.Examples = append(joined.Examples, se.Examples...)
	}
	for _, o := range outs {
		joined.Out = append(joined.Out, *o)
	}
	sort.Slice(joined.Out, func(i, j int) bool { return joined.Out[i].Event < joined.Out[j].Event })
	sort.Slice(joined.In, func(i, j int) bool { return joined.In[i].ID < joined.In[j].ID })
	joined.Architecture = strings.Join(ids, ", ")
	if len(problems) > 0 {
		return joined, fmt.Errorf("the coordinating documents contradict each other for %q:\n  %s", doc.ID,
			strings.Join(problems, "\n  "))
	}
	return joined, nil
}

// systemOf is the key a names this document by: the system whose release
// names its id, or whose document is the file at docPath. Exactly one.
func (a *Architecture) systemOf(doc *Document, docPath string) (string, error) {
	got, err := filepath.Abs(docPath)
	if err != nil {
		return "", err
	}
	var found []string
	for k, sys := range a.Systems {
		switch {
		case sys.Release != nil && sys.Release.System == doc.ID:
			found = append(found, k)
		case sys.Document != nil:
			if want, err := filepath.Abs(filepath.Join(a.dir, sys.Document.Path)); err == nil && samePath(want, got) {
				found = append(found, k)
			}
		}
	}
	sort.Strings(found)
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("the coordinating document %q names no system that is this document (%q, %s): "+
			"name it with --system", a.ID, doc.ID, docPath)
	default:
		return "", fmt.Errorf("the coordinating document %q names this document as more than one system (%s): "+
			"name one with --system", a.ID, strings.Join(found, ", "))
	}
}
