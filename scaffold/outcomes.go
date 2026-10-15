package scaffold

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Outcome is one alternative a command may produce (schema 3.3.0/3.12.0/
// 3.13.0). The alternatives are tried in order after the refusals; the
// first whose condition holds is the result, and the last has none (the
// default), so a decision always has exactly one answer.
type Outcome struct {
	// Events are emitted together, in this order.
	Events []string `json:"events"`
	// When holds when any group holds (each group: all its conditions).
	When [][]RefusalCondition `json:"when,omitempty"`
	// ByHand: the condition is a hand-written hook (when: "hand").
	ByHand bool `json:"byHand,omitempty"`
	// Set fixes field values on this alternative's events: event -> field
	// -> literal.
	Set map[string]map[string]any `json:"set,omitempty"`
	// Repeated events occur zero or more times, one per item a hand-written
	// hook returns, in their place in Events (schema 3.13.0).
	Repeated []string `json:"repeated,omitempty"`
}

// OutcomeHook is the hand-written condition of command c's outcome i.
func OutcomeHook(c Command, i int) string { return fmt.Sprintf("%sOutcome%d", c.Name, i) }

// RepeatedHook returns the items of a repeated event in command c's
// outcome i.
func RepeatedHook(c Command, i int, event string) string {
	return fmt.Sprintf("%sOutcome%d%s", c.Name, i, event)
}

// Hooks lists every hand-written hook the domain's deciders call, sorted:
// what its hand file (JS <aggregate>.hand.js, Go <aggregate>_hand.go) must
// define.
func (d Domain) Hooks() []string {
	var out []string
	for _, c := range d.Commands {
		for i, o := range c.Outcomes {
			if o.ByHand {
				out = append(out, OutcomeHook(c, i))
			}
			for _, e := range o.Repeated {
				out = append(out, RepeatedHook(c, i, e))
			}
		}
	}
	sort.Strings(out)
	return out
}

// HandFile is the JS companion a decider with hooks loads from.
func (d Domain) HandFile() string { return d.Aggregate + ".hand.js" }

// checkOutcomes: an outcome's events are the command's; its conditions are
// checked as refusals' are.
func (d Domain) checkOutcomes(add func(string, ...any)) {
	for _, c := range d.Commands {
		names := map[string]bool{}
		for _, e := range c.Events {
			names[e.Name] = true
		}
		for i, o := range c.Outcomes {
			for _, e := range o.Events {
				if !names[e] {
					add("command %q outcome %d emits %q, which the command does not produce", c.Name, i, e)
				}
			}
			if o.ByHand && len(o.When) > 0 {
				add("command %q outcome %d is both declared and hand-written", c.Name, i)
			}
		}
	}
}

func (c Command) outcomeConditions() []Refusal {
	var out []Refusal
	for i, o := range c.Outcomes {
		for _, g := range o.When {
			out = append(out, Refusal{Message: fmt.Sprintf("outcome %d", i), All: g})
		}
	}
	return out
}

// ---- JS ----

func jsOutcomeTest(c Command, i int, o Outcome) string {
	if o.ByHand {
		return OutcomeHook(c, i) + "(state, command)"
	}
	if len(o.When) == 0 {
		return ""
	}
	groups := make([]string, len(o.When))
	for g, conds := range o.When {
		parts := make([]string, len(conds))
		for k, cond := range conds {
			parts[k] = jsRefusalCondition(cond)
		}
		groups[g] = "(" + strings.Join(parts, " && ") + ")"
	}
	return strings.Join(groups, " || ")
}

func jsOutcomeEvents(c Command, i int, o Outcome) string {
	byName := map[string]Event{}
	for _, e := range c.Events {
		byName[e.Name] = e
	}
	parts := make([]string, 0, len(o.Events))
	for _, name := range o.Events {
		if contains(o.Repeated, name) {
			parts = append(parts, fmt.Sprintf("...%s(state, command).map(function (data) { return { type: '%s', data: data }; })",
				RepeatedHook(c, i, name), name))
			continue
		}
		data := payloadLiteral(byName[name].Fields)
		if set := o.Set[name]; len(set) > 0 {
			fixed, _ := json.Marshal(set)
			data = "Object.assign(" + data + ", " + string(fixed) + ")"
		}
		parts = append(parts, fmt.Sprintf("{ type: '%s', data: %s }", name, data))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// writeJSOutcomes emits a command's alternatives, in order, after its
// refusals.
func writeJSOutcomes(b *strings.Builder, c Command) {
	b.WriteString("      // outcomes (schema 3.12.0), in order: the first whose condition holds is the result\n")
	for i, o := range c.Outcomes {
		if test := jsOutcomeTest(c, i, o); test != "" {
			fmt.Fprintf(b, "      if (%s) { return %s; }\n", test, jsOutcomeEvents(c, i, o))
			continue
		}
		fmt.Fprintf(b, "      return %s;\n", jsOutcomeEvents(c, i, o))
	}
}

// jsHandExample is a starting point for the hand file: every hook the
// decider calls, as a stub that refuses to guess. It is written beside the
// generated files as <aggregate>.hand.js.example, which nothing loads.
func (d Domain) jsHandExample() string {
	var b strings.Builder
	fmt.Fprintf(&b, "// %s: the hand-written hooks %s.js calls. Copy this to %s and write each\n", d.HandFile(), d.Aggregate, d.HandFile())
	b.WriteString("// one. They run in the decider's own VM: they see the folded state and the\n")
	b.WriteString("// command, and nothing else (no Date, no I/O, no read models). The decider\n")
	b.WriteString("// does not load until every hook is defined.\n\n")
	for _, c := range d.Commands {
		for i, o := range c.Outcomes {
			if o.ByHand {
				fmt.Fprintf(&b, "// Whether %s emits [%s] (outcome %d, when: \"hand\").\n", c.Name, strings.Join(o.Events, ", "), i)
				fmt.Fprintf(&b, "function %s(state, command) {\n  throw new Error('write %s');\n}\n\n", OutcomeHook(c, i), OutcomeHook(c, i))
			}
			for _, e := range o.Repeated {
				fmt.Fprintf(&b, "// The %s events %s emits in outcome %d: one data object per item, in\n// order (none is fine; schema 3.13.0).\n", e, c.Name, i)
				fmt.Fprintf(&b, "function %s(state, command) {\n  throw new Error('write %s');\n}\n\n", RepeatedHook(c, i, e), RepeatedHook(c, i, e))
			}
		}
	}
	return b.String()
}

// JSHandStandIn is a hand file whose every hook throws a marker the
// verifier recognises: a scenario that reaches a hook is decided by hand,
// which only a host's own hooks can check.
func (d Domain) JSHandStandIn() string {
	var b strings.Builder
	for _, h := range d.Hooks() {
		fmt.Fprintf(&b, "function %s(state, command) { throw new Error(%q); }\n", h, HandMarker+h)
	}
	return b.String()
}

// GoHandStandIn is JSHandStandIn for the Go target: <aggregate>_hand.go
// with every hook panicking HandMarker+name, for verify's Go harness to
// recover and report HAND. The same skeleton as goHandExample, so the
// signatures can't drift.
func (d Domain) GoHandStandIn(pkg string) string {
	src := d.goHandExample(pkg)
	for _, h := range d.Hooks() {
		src = strings.Replace(src, fmt.Sprintf("panic(%q)", "write "+h), fmt.Sprintf("panic(%q)", HandMarker+h), 1)
	}
	return src
}

// HandMarker prefixes the error a stand-in hook throws.
const HandMarker = "$hand:"

// ---- Go ----

// goOutcomeEvent writes the code building one non-repeated event of an
// outcome into variable v, as a single event is built (its fields from the
// command's payload), with the outcome's set values on top.
func goOutcomeEvent(b *strings.Builder, v string, e Event, set map[string]any, fieldGoType func(name, typ string) string) {
	if len(e.Fields) == 0 {
		fmt.Fprintf(b, "\t\t\t\t%s := events.NewEvent{Type: %s, Data: json.RawMessage(\"{}\")}\n", v, e.Name)
		return
	}
	fmt.Fprintf(b, "\t\t\t\tvar %sData struct {\n", v)
	for _, f := range e.Fields {
		fmt.Fprintf(b, "\t\t\t\t\t%s %s `json:%q`\n", ExportName(f.Name), fieldGoType(f.Name, f.Type), f.Name)
	}
	b.WriteString("\t\t\t\t}\n")
	fmt.Fprintf(b, "\t\t\t\tif err := json.Unmarshal(cmd.Payload, &%sData); err != nil {\n\t\t\t\t\treturn nil, err\n\t\t\t\t}\n", v)
	names := make([]string, 0, len(set))
	for k := range set {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Fprintf(b, "\t\t\t\t%sData.%s = %s\n", v, ExportName(k), goLiteral(set[k]))
	}
	fmt.Fprintf(b, "\t\t\t\t%sBytes, err := json.Marshal(%sData)\n\t\t\t\tif err != nil {\n\t\t\t\t\treturn nil, err\n\t\t\t\t}\n", v, v)
	fmt.Fprintf(b, "\t\t\t\t%s := events.NewEvent{Type: %s, Data: %sBytes}\n", v, e.Name, v)
}

func goLiteral(v any) string {
	switch x := v.(type) {
	case string:
		return fmt.Sprintf("%q", x)
	case float64:
		return fmt.Sprintf("%v", x)
	case bool:
		return fmt.Sprintf("%v", x)
	}
	return "nil"
}

func goOutcomeTest(c Command, i int, o Outcome) string {
	if o.ByHand {
		return OutcomeHook(c, i) + "(s, cmd)"
	}
	if len(o.When) == 0 {
		return ""
	}
	groups := make([]string, len(o.When))
	for g, conds := range o.When {
		parts := make([]string, len(conds))
		for k, cond := range conds {
			parts[k] = goRefusalCondition(cond)
		}
		groups[g] = "(" + strings.Join(parts, " && ") + ")"
	}
	return strings.Join(groups, " || ")
}

// writeGoOutcomes emits a command's alternatives, in order, after its
// refusals: each a block returning its events.
func writeGoOutcomes(b *strings.Builder, c Command, fieldGoType func(name, typ string) string) {
	b.WriteString("\t\t\t\t// outcomes (schema 3.12.0), in order: the first whose condition holds is the result\n")
	readsCommand := false
	for _, o := range c.Outcomes {
		for _, g := range o.When {
			for _, cond := range g {
				readsCommand = readsCommand || cond.Side == "command" || (cond.Operand != nil && cond.Operand.Side == "command")
			}
		}
	}
	if readsCommand && !c.readsCommandInRefusals() {
		b.WriteString("\t\t\t\trefusalCommand := refusalPayload(cmd.Payload)\n")
	}
	byName := map[string]Event{}
	for _, e := range c.Events {
		byName[e.Name] = e
	}
	for i, o := range c.Outcomes {
		test := goOutcomeTest(c, i, o)
		if test != "" {
			fmt.Fprintf(b, "\t\t\t\tif %s {\n", test)
		} else {
			b.WriteString("\t\t\t\t{\n")
		}
		b.WriteString("\t\t\t\tvar out []events.NewEvent\n")
		for k, name := range o.Events {
			if contains(o.Repeated, name) {
				fmt.Fprintf(b, "\t\t\t\tfor _, item := range %s(s, cmd) {\n", RepeatedHook(c, i, name))
				b.WriteString("\t\t\t\t\tdata, err := json.Marshal(item)\n\t\t\t\t\tif err != nil {\n\t\t\t\t\t\treturn nil, err\n\t\t\t\t\t}\n")
				fmt.Fprintf(b, "\t\t\t\t\tout = append(out, events.NewEvent{Type: %s, Data: data})\n\t\t\t\t}\n", name)
				continue
			}
			v := fmt.Sprintf("e%d", k)
			goOutcomeEvent(b, v, byName[name], o.Set[name], fieldGoType)
			fmt.Fprintf(b, "\t\t\t\tout = append(out, %s)\n", v)
		}
		b.WriteString("\t\t\t\treturn out, nil\n\t\t\t\t}\n")
	}
}

func (c Command) readsCommandInRefusals() bool { return len(c.Refusals) > 0 }

// goHandExample is the Go hand file's starting point,
// <aggregate>_hand.go.example: every hook the decider calls, as a stub. A
// hook missing from the real <aggregate>_hand.go is a build error.
func (d Domain) goHandExample(pkg string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	b.WriteString("import \"github.com/jamestryand/pocketcqrs/decider\"\n\n")
	fmt.Fprintf(&b, "// The hand-written hooks %s.go calls: copy this to %s_hand.go and write each.\n", d.Aggregate, d.Aggregate)
	b.WriteString("// Like Decide, they read only the folded state and the command: no clock, I/O\n// or read models.\n\n")
	for _, c := range d.Commands {
		for i, o := range c.Outcomes {
			if o.ByHand {
				fmt.Fprintf(&b, "// %s: whether %s emits [%s] (outcome %d, when: \"hand\").\n", OutcomeHook(c, i), c.Name, strings.Join(o.Events, ", "), i)
				fmt.Fprintf(&b, "func %s(s State, cmd decider.Command) bool {\n\tpanic(\"write %s\")\n}\n\n", OutcomeHook(c, i), OutcomeHook(c, i))
			}
			for _, e := range o.Repeated {
				fmt.Fprintf(&b, "// %s: the %s events %s emits in outcome %d, one per item, in order\n// (none is fine; schema 3.13.0). Each item becomes one event's data.\n",
					RepeatedHook(c, i, e), e, c.Name, i)
				fmt.Fprintf(&b, "func %s(s State, cmd decider.Command) []map[string]any {\n\tpanic(\"write %s\")\n}\n\n", RepeatedHook(c, i, e), RepeatedHook(c, i, e))
			}
		}
	}
	return b.String()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
