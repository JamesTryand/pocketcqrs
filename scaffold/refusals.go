package scaffold

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Refusal is one refuseWhen rule (schema 3.10.0; numeric operators
// 3.11.0): when every condition holds, the command is refused with Message.
// Rules are checked in order, after the existence checks; the first that
// holds refuses.
type Refusal struct {
	Message string             `json:"message"`
	All     []RefusalCondition `json:"all"`
}

// RefusalCondition reads one field, of the aggregate's own state (folded
// from its own events) or of the command, and applies one operator: set,
// notSet, equals, notEquals, contains, lessThan, atMost, greaterThan or
// atLeast.
type RefusalCondition struct {
	Side    string          `json:"side"` // "state" | "command"
	Field   string          `json:"field"`
	Op      string          `json:"op"`
	Operand *RefusalOperand `json:"operand,omitempty"`
}

// RefusalOperand is what a condition compares with: a literal (a string,
// number or bool) or the value of another state or command field.
type RefusalOperand struct {
	Side    string `json:"side,omitempty"`
	Field   string `json:"field,omitempty"`
	Literal any    `json:"literal,omitempty"`
}

var numericOps = map[string]bool{"lessThan": true, "atMost": true, "greaterThan": true, "atLeast": true}

func (d Domain) hasRefusals() bool {
	for _, c := range d.Commands {
		if len(c.Refusals) > 0 {
			return true
		}
	}
	return false
}

// checkRefusals reports every rule that names a field that isn't there, or
// compares one in a way it can't be compared: a rule the generated decider
// silently skipped would let through exactly what the model refuses. A
// state field is one of the aggregate's event fields; a command field one
// the command declares. Personal data may only be tested for set/notSet,
// the numeric operators compare numbers, and contains needs a list.
func (d Domain) checkRefusals(add func(string, ...any)) {
	state := map[string]Field{}
	for _, c := range d.Commands {
		for _, e := range c.Events {
			for _, f := range e.Fields {
				state[f.Name] = f
			}
		}
	}
	d.checkOwnership(add, state)
	for _, c := range d.Commands {
		payload := map[string]Field{}
		for _, f := range c.Fields {
			payload[f.Name] = f
		}
		lookup := func(side, field, rule string) (Field, bool) {
			fields := payload
			if side == "state" {
				fields = state
			}
			f, ok := fields[field]
			if !ok {
				add("command %q: refusal %q names %s field %q, which %s", c.Name, rule, side, field,
					map[string]string{"state": "no event of this aggregate carries", "command": "the command does not declare"}[side])
			}
			return f, ok
		}
		for _, r := range c.Refusals {
			for _, cond := range r.All {
				f, ok := lookup(cond.Side, cond.Field, r.Message)
				if !ok || cond.Op == "set" || cond.Op == "notSet" {
					continue
				}
				if f.PII {
					add("command %q: refusal %q compares personal-data field %q; a rule may only test it for set or notSet", c.Name, r.Message, cond.Field)
					continue
				}
				if numericOps[cond.Op] && f.Type != "number" {
					add("command %q: refusal %q: %s compares numbers, but %q is %s", c.Name, r.Message, cond.Op, cond.Field, f.Type)
				}
				if cond.Op == "contains" && f.Type != "json" {
					add("command %q: refusal %q: contains needs a list, but %q is %s", c.Name, r.Message, cond.Field, f.Type)
				}
				if o := cond.Operand; o != nil && o.Side != "" {
					of, ok := lookup(o.Side, o.Field, r.Message)
					if ok && of.PII {
						add("command %q: refusal %q compares personal-data field %q; a rule may only test it for set or notSet", c.Name, r.Message, o.Field)
					} else if ok && numericOps[cond.Op] && of.Type != "number" {
						add("command %q: refusal %q: %s compares numbers, but %q is %s", c.Name, r.Message, cond.Op, o.Field, of.Type)
					}
				}
			}
		}
	}
}

// Ownership is requiredOwnership.stateField (schema 3.15.0).
type Ownership struct {
	StateField string `json:"stateField"`
	Message    string `json:"message"`
}

// checkOwnership: the owner field is a text field of the aggregate's state.
func (d Domain) checkOwnership(add func(string, ...any), state map[string]Field) {
	for _, c := range d.Commands {
		if c.OwnedBy == nil {
			continue
		}
		f, ok := state[c.OwnedBy.StateField]
		switch {
		case !ok:
			add("command %q: requiredOwnership.stateField %q is not a field any event of this aggregate carries", c.Name, c.OwnedBy.StateField)
		case f.Type != "text":
			add("command %q: requiredOwnership.stateField %q is %s; an owner is text", c.Name, c.OwnedBy.StateField, f.Type)
		}
	}
}

// ---- JS ----

func writeJSOwnership(b *strings.Builder, c Command) {
	if c.OwnedBy == nil {
		return
	}
	field, _ := json.Marshal(c.OwnedBy.StateField)
	msg, _ := json.Marshal(c.OwnedBy.Message)
	b.WriteString("      // requiredOwnership.stateField (schema 3.15.0): only the owner its own state\n")
	b.WriteString("      // names; anyone else is refused as forbidden (an Error named Forbidden)\n")
	fmt.Fprintf(b, "      if (state[%s] !== command.actor) { var forbidden = new Error(%s); forbidden.name = 'Forbidden'; throw forbidden; }\n", field, msg)
}

// jsRefusalHelpers are the rule semantics both targets share: a missing or
// null value is unset; text, numbers and bools compare by value and type; a
// numeric comparison with an unset (or non-numeric) value never holds (NaN
// compares false); contains asks whether a list holds a value.
const jsRefusalHelpers = `// refuseWhen (schema 3.10.0/3.11.0): a missing or null value is unset; text,
// numbers and bools compare by value; a numeric comparison with an unset
// value never holds.
function refusalSet(v) { return v !== undefined && v !== null; }
function refusalEquals(a, b) { return refusalSet(a) ? a === b : !refusalSet(b); }
function refusalCompare(a, b) { return (typeof a === 'number' && typeof b === 'number') ? a - b : NaN; }
function refusalContains(list, item) { return Array.isArray(list) && list.some(function (e) { return refusalEquals(e, item); }); }

`

func jsRefusalField(side, field string) string {
	name, _ := json.Marshal(field)
	if side == "state" {
		return "state[" + string(name) + "]"
	}
	return "(command.payload || {})[" + string(name) + "]"
}

func jsRefusalOperand(o *RefusalOperand) string {
	if o == nil {
		return "undefined"
	}
	if o.Side != "" {
		return jsRefusalField(o.Side, o.Field)
	}
	lit, _ := json.Marshal(o.Literal)
	return string(lit)
}

func jsRefusalCondition(c RefusalCondition) string {
	v, o := jsRefusalField(c.Side, c.Field), jsRefusalOperand(c.Operand)
	switch c.Op {
	case "set":
		return "refusalSet(" + v + ")"
	case "notSet":
		return "!refusalSet(" + v + ")"
	case "equals":
		return "refusalEquals(" + v + ", " + o + ")"
	case "notEquals":
		return "!refusalEquals(" + v + ", " + o + ")"
	case "contains":
		return "refusalContains(" + v + ", " + o + ")"
	case "lessThan":
		return "refusalCompare(" + v + ", " + o + ") < 0"
	case "atMost":
		return "refusalCompare(" + v + ", " + o + ") <= 0"
	case "greaterThan":
		return "refusalCompare(" + v + ", " + o + ") > 0"
	case "atLeast":
		return "refusalCompare(" + v + ", " + o + ") >= 0"
	}
	return "false"
}

// writeJSRefusals emits a command's rules, in order, after its existence
// checks.
func writeJSRefusals(b *strings.Builder, c Command) {
	if len(c.Refusals) == 0 {
		return
	}
	b.WriteString("      // refuseWhen (schema 3.10.0), in order: the first rule that holds refuses\n")
	for _, r := range c.Refusals {
		conds := make([]string, len(r.All))
		for i, cond := range r.All {
			conds[i] = jsRefusalCondition(cond)
		}
		msg, _ := json.Marshal(r.Message)
		fmt.Fprintf(b, "      if (%s) { throw new Error(%s); }\n", strings.Join(conds, " && "), msg)
	}
}

// ---- Go ----

// goRefusalHelpers mirror jsRefusalHelpers for the Go target. The rules
// read Known, the aggregate's event values as they were folded (a field no
// event has set is absent), because the typed State can't tell an unset
// field from a zero one.
const goRefusalHelpers = `
// refuseWhen (schema 3.10.0/3.11.0): a missing or null value is unset; text,
// numbers and bools compare by value; a numeric comparison with an unset
// value never holds. Known is folded copy-on-write, so an earlier State is
// never changed by a later event.
func refusalFold(known map[string]any, data json.RawMessage) map[string]any {
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil || len(fields) == 0 {
		return known
	}
	out := make(map[string]any, len(known)+len(fields))
	for k, v := range known {
		out[k] = v
	}
	for k, v := range fields {
		out[k] = v
	}
	return out
}

func refusalPayload(raw json.RawMessage) map[string]any {
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	return fields
}

func refusalSet(v any) bool { return v != nil }

func refusalEquals(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case float64:
		y, ok := b.(float64)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	}
	return false
}

func refusalCompare(a, b any, holds func(int) bool) bool {
	x, okx := a.(float64)
	y, oky := b.(float64)
	if !okx || !oky {
		return false
	}
	switch {
	case x < y:
		return holds(-1)
	case x > y:
		return holds(1)
	}
	return holds(0)
}

func refusalContains(list, item any) bool {
	items, ok := list.([]any)
	if !ok {
		return false
	}
	for _, e := range items {
		if refusalEquals(e, item) {
			return true
		}
	}
	return false
}
`

func goRefusalField(side, field string) string {
	if side == "state" {
		return fmt.Sprintf("s.Known[%q]", field)
	}
	return fmt.Sprintf("refusalCommand[%q]", field)
}

func goRefusalOperand(o *RefusalOperand) string {
	if o == nil {
		return "nil"
	}
	if o.Side != "" {
		return goRefusalField(o.Side, o.Field)
	}
	switch v := o.Literal.(type) {
	case string:
		return fmt.Sprintf("any(%q)", v)
	case float64:
		return fmt.Sprintf("any(float64(%v))", v)
	case int:
		return fmt.Sprintf("any(float64(%d))", v)
	case bool:
		return fmt.Sprintf("any(%v)", v)
	}
	return "nil"
}

func goRefusalCondition(c RefusalCondition) string {
	v, o := goRefusalField(c.Side, c.Field), goRefusalOperand(c.Operand)
	cmp := func(test string) string {
		return fmt.Sprintf("refusalCompare(%s, %s, func(c int) bool { return %s })", v, o, test)
	}
	switch c.Op {
	case "set":
		return "refusalSet(" + v + ")"
	case "notSet":
		return "!refusalSet(" + v + ")"
	case "equals":
		return "refusalEquals(" + v + ", " + o + ")"
	case "notEquals":
		return "!refusalEquals(" + v + ", " + o + ")"
	case "contains":
		return "refusalContains(" + v + ", " + o + ")"
	case "lessThan":
		return cmp("c < 0")
	case "atMost":
		return cmp("c <= 0")
	case "greaterThan":
		return cmp("c > 0")
	case "atLeast":
		return cmp("c >= 0")
	}
	return "false"
}

func writeGoOwnership(b *strings.Builder, c Command) {
	if c.OwnedBy == nil {
		return
	}
	b.WriteString("\t\t\t\t// requiredOwnership.stateField (schema 3.15.0): only the owner its own\n")
	b.WriteString("\t\t\t\t// state names; anyone else is refused as forbidden\n")
	fmt.Fprintf(b, "\t\t\t\tif s.%s != cmd.Actor {\n\t\t\t\t\treturn nil, decider.Forbidden(%q)\n\t\t\t\t}\n",
		ExportName(c.OwnedBy.StateField), c.OwnedBy.Message)
}

func writeGoRefusals(b *strings.Builder, c Command) {
	if len(c.Refusals) == 0 {
		return
	}
	b.WriteString("\t\t\t\t// refuseWhen (schema 3.10.0), in order: the first rule that holds refuses\n")
	b.WriteString("\t\t\t\trefusalCommand := refusalPayload(cmd.Payload)\n")
	readsCommand := false
	for _, r := range c.Refusals {
		conds := make([]string, len(r.All))
		for i, cond := range r.All {
			conds[i] = goRefusalCondition(cond)
			readsCommand = readsCommand || cond.Side == "command" || (cond.Operand != nil && cond.Operand.Side == "command")
		}
		fmt.Fprintf(b, "\t\t\t\tif %s {\n\t\t\t\t\treturn nil, fmt.Errorf(\"%%s\", %q)\n\t\t\t\t}\n", strings.Join(conds, " && "), r.Message)
	}
	if !readsCommand {
		b.WriteString("\t\t\t\t_ = refusalCommand\n")
	}
}
