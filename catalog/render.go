package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// JSON renders the catalog as indented JSON.
func (c *Catalog) JSON() ([]byte, error) {
	return json.MarshalIndent(c, "", "  ")
}

// Markdown renders the catalog as a Markdown document with an embedded
// Mermaid diagram.
func (c *Catalog) Markdown() string {
	var b strings.Builder

	fmt.Fprintf(&b, "# PocketCQRS catalog\n\n")
	fmt.Fprintf(&b, "_Generated %s · mode: **%s** · %d events · %d streams · %d pending dead letter(s)_\n\n",
		c.GeneratedAt, c.Mode, c.Totals.Events, c.Totals.Streams, c.Totals.DeadLettersPending)

	b.WriteString("## Aggregates\n\n")
	b.WriteString("| aggregate | origin | streams | events (empirical) | declared handles | transforms |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for _, a := range c.Aggregates {
		var evts []string
		for _, e := range a.Events {
			evts = append(evts, fmt.Sprintf("%s ×%d (v%d)", e.Type, e.Count, e.MaxVersion))
		}
		fmt.Fprintf(&b, "| `%s` | %s | %d | %s | %s | %s |\n",
			a.Name, a.Origin, a.Streams, strings.Join(evts, ", "), strings.Join(a.Handles, ", "),
			strings.Join(a.Transforms, ", "))
	}

	b.WriteString("\n## Consumers\n\n")
	b.WriteString("| consumer | kind | triggers | collections | checkpoint |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, cons := range c.Consumers {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %d |\n",
			cons.Name, cons.Kind, strings.Join(cons.EventTypes, ", "),
			strings.Join(cons.Collections, ", "), cons.Checkpoint)
	}

	b.WriteString("\n## Collections\n\n")
	b.WriteString("| collection | guarded | owner | key | fields |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, col := range c.Collections {
		guarded := ""
		if col.Guarded {
			guarded = "yes"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n",
			col.Name, guarded, col.Owner, col.Key, strings.Join(col.Fields, ", "))
	}

	b.WriteString("\n## Functions\n\n")
	if len(c.Functions.HTTP) > 0 {
		fmt.Fprintf(&b, "- HTTP: `%s` (`/api/fn/<name>`)\n", strings.Join(c.Functions.HTTP, "`, `"))
	}
	for _, job := range c.Functions.Cron {
		fmt.Fprintf(&b, "- cron: `%s` (`%s`)\n", job.Name, job.Schedule)
	}
	if len(c.Functions.HTTP) == 0 && len(c.Functions.Cron) == 0 {
		b.WriteString("- none registered\n")
	}

	b.WriteString("\n## Flows (empirical, from causation metadata)\n\n")
	if len(c.Flows) == 0 {
		b.WriteString("- none observed\n")
	} else {
		b.WriteString("| reactor | cause | target | count |\n")
		b.WriteString("| --- | --- | --- | --- |\n")
		for _, f := range c.Flows {
			fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %d |\n", f.Reactor, f.Cause, f.Target, f.Count)
		}
	}

	b.WriteString("\n## Diagram\n\n```mermaid\n")
	b.WriteString(c.Mermaid())
	b.WriteString("```\n")
	return b.String()
}

// Mermaid renders a flowchart of the platform: aggregates emit event types
// to their consumers; projections own collections; reactors dispatch to
// target aggregates (empirical, from the log).
func (c *Catalog) Mermaid() string {
	var b strings.Builder
	b.WriteString("flowchart LR\n")

	// which aggregates empirically produce each event type
	producers := map[string][]string{}
	for _, a := range c.Aggregates {
		for _, e := range a.Events {
			producers[e.Type] = append(producers[e.Type], a.Name)
		}
	}

	for _, a := range c.Aggregates {
		fmt.Fprintf(&b, "  %s([\"%s\"])\n", nodeID("agg", a.Name), a.Name)
	}

	// consumer nodes + trigger edges (declared where available)
	for _, cons := range c.Consumers {
		id := nodeID("cons", cons.Name)
		fmt.Fprintf(&b, "  %s{{\"%s\"}}\n", id, cons.Name)
		types := cons.EventTypes
		if len(types) == 0 && cons.Kind == "reactor" {
			// reactor triggers are empirical (flows below)
			continue
		}
		for _, t := range types {
			for _, agg := range producers[t] {
				fmt.Fprintf(&b, "  %s -- \"%s\" --> %s\n", nodeID("agg", agg), t, id)
			}
		}
	}

	// ownership edges
	for _, cons := range c.Consumers {
		for _, col := range cons.Collections {
			colID := nodeID("col", col)
			fmt.Fprintf(&b, "  %s[(\"%s\")]\n", colID, col)
			fmt.Fprintf(&b, "  %s --> %s\n", nodeID("cons", cons.Name), colID)
		}
	}

	// reactor flows (empirical)
	for _, f := range c.Flows {
		causeAgg, causeType, _ := strings.Cut(f.Cause, "/")
		targetAgg, targetType, _ := strings.Cut(f.Target, "/")
		fmt.Fprintf(&b, "  %s -- \"%s\" --> %s\n", nodeID("agg", causeAgg), causeType, nodeID("cons", f.Reactor))
		fmt.Fprintf(&b, "  %s -- \"%s\" --> %s\n", nodeID("cons", f.Reactor), targetType, nodeID("agg", targetAgg))
	}

	return b.String()
}

// nodeID makes a Mermaid-safe node id (alphanumerics + underscore).
func nodeID(prefix, name string) string {
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteByte('_')
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Skeleton renders a domain-doc skeleton for one aggregate, following the
// docs/domains convention. Sections that cannot be introspected (commands,
// intent) are left as TODO rows — draft docs validate; lint flags the rest.
func (c *Catalog) Skeleton(a Aggregate) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s\n\n", a.Name)
	b.WriteString("<!-- TODO: one paragraph — the business capability this aggregate owns -->\n\n")

	// Commands are the one part of a slice that cannot be recovered from the
	// log — they leave no trace there — so the skeleton can only fill this
	// in for an aggregate that DECLARES them (//@commands, or Commands on a
	// Go decider). Without a declaration the TODO row stands, which is the
	// honest answer rather than a guess from event names.
	b.WriteString("## Commands\n\n")
	b.WriteString("| command | payload | intent | rejects when |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	if len(a.Commands) == 0 {
		b.WriteString("| `TODO` | | | |\n")
		b.WriteString("\n<!-- declare //@commands (JS) or Commands (Go) and this table fills itself in -->\n")
	}
	for _, cmd := range a.Commands {
		fmt.Fprintf(&b, "| `%s` | | | |\n", cmd)
	}
	b.WriteString("\n")

	b.WriteString("## Events\n\n")
	b.WriteString("| event | data | since version | notes |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	if len(a.Events) == 0 {
		b.WriteString("| `TODO` | | | |\n")
	}
	for _, e := range a.Events {
		fmt.Fprintf(&b, "| `%s` |  | v%d | ×%d in the log |\n", e.Type, e.MaxVersion, e.Count)
	}

	b.WriteString("\n## Read models\n\n")
	b.WriteString("| collection | owner | shape | notes |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	found := false
	for _, cons := range c.Consumers {
		if len(cons.Collections) == 0 {
			continue
		}
		// a projection is this aggregate's read-model owner when it
		// triggers on the aggregate's event types
		for _, t := range cons.EventTypes {
			if eventIn(a.Events, t) {
				for _, col := range cons.Collections {
					fmt.Fprintf(&b, "| `%s` | projection `%s` |  | |\n", col, cons.Name)
					found = true
				}
				break
			}
		}
	}
	if !found {
		b.WriteString("| `TODO` | | | |\n")
	}

	b.WriteString("\n## Flows (reactors/sagas)\n\n")
	found = false
	for _, f := range c.Flows {
		causeAgg, _, _ := strings.Cut(f.Cause, "/")
		targetAgg, _, _ := strings.Cut(f.Target, "/")
		if causeAgg == a.Name || targetAgg == a.Name {
			fmt.Fprintf(&b, "- on `%s` → `%s` (reactor `%s`, ×%d)\n", f.Cause, f.Target, f.Reactor, f.Count)
			found = true
		}
	}
	if !found {
		b.WriteString("- none observed\n")
	}

	b.WriteString("\n## Scenarios\n\n")
	b.WriteString("- given nothing → `TODO` → `TODO`\n")

	b.WriteString("\n## Implementation\n\n")
	if a.Origin == "js" {
		fmt.Fprintf(&b, "- decider: `pb_functions/%s.js` (JS, tier 3)\n", a.Name)
	} else {
		fmt.Fprintf(&b, "- decider: `aggregates/%s.go` (Go)\n", a.Name)
	}
	if len(a.Transforms) > 0 {
		fmt.Fprintf(&b, "- transforms: %s\n", strings.Join(a.Transforms, ", "))
	}
	return b.String()
}

// WriteSkeletons writes docs/domains/<aggregate>.md skeletons for every
// aggregate, skipping existing files unless force. Returns written paths.
func (c *Catalog) WriteSkeletons(dir string, force bool) (written []string, skipped []string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	for _, a := range c.Aggregates {
		path := filepath.Join(dir, a.Name+".md")
		if !force {
			if _, err := os.Stat(path); err == nil {
				skipped = append(skipped, path)
				continue
			}
		}
		if err := os.WriteFile(path, []byte(c.Skeleton(a)), 0o644); err != nil {
			return written, skipped, err
		}
		written = append(written, path)
	}
	sort.Strings(written)
	sort.Strings(skipped)
	return written, skipped, nil
}

func eventIn(events []EventType, t string) bool {
	for _, e := range events {
		if e.Type == t {
			return true
		}
	}
	return false
}
