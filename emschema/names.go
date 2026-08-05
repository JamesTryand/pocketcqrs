package emschema

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// ---- scenario accessors ----
//
// `when` and `then` are raw until the kind selects a shape, because the
// source schema discriminates on `kind` and one field meaning three shapes
// is exactly what a typed accessor should hide from callers.

// CommandRef decodes a stateChange or error scenario's `when`.
func (s Scenario) CommandRef() (CommandRef, error) {
	var out CommandRef
	if len(s.When) == 0 {
		return out, fmt.Errorf("a %s scenario needs a `when` naming a command", s.Kind)
	}
	if err := json.Unmarshal(s.When, &out); err != nil {
		return out, fmt.Errorf("`when` is not a command reference: %w", err)
	}
	if out.CommandID == "" {
		return out, fmt.Errorf("`when` names no commandId")
	}
	return out, nil
}

// ReadModelQuery decodes a stateView scenario's `when`.
func (s Scenario) ReadModelQuery() (ReadModelQuery, error) {
	var out ReadModelQuery
	if len(s.When) == 0 {
		return out, fmt.Errorf("a stateView scenario needs a `when` naming a read model")
	}
	if err := json.Unmarshal(s.When, &out); err != nil {
		return out, fmt.Errorf("`when` is not a read-model query: %w", err)
	}
	if out.ReadModelID == "" {
		return out, fmt.Errorf("`when` names no readModelId")
	}
	return out, nil
}

// EventsThen decodes a stateChange scenario's `then`.
func (s Scenario) EventsThen() (EventsThen, error) {
	var out EventsThen
	if len(s.Then) == 0 {
		return out, fmt.Errorf("a stateChange scenario needs a `then` listing events")
	}
	if err := json.Unmarshal(s.Then, &out); err != nil {
		return out, fmt.Errorf("`then` is not an events expectation: %w", err)
	}
	if len(out.Events) == 0 {
		return out, fmt.Errorf("`then` lists no events")
	}
	return out, nil
}

// ErrorThen decodes an error scenario's `then`.
func (s Scenario) ErrorThen() (ErrorThen, error) {
	var out ErrorThen
	if len(s.Then) == 0 {
		return out, fmt.Errorf("an error scenario needs a `then` describing the refusal")
	}
	if err := json.Unmarshal(s.Then, &out); err != nil {
		return out, fmt.Errorf("`then` is not an error expectation: %w", err)
	}
	if out.Error.Message == "" {
		return out, fmt.Errorf("`then.error` has no message")
	}
	return out, nil
}

// ResultThen decodes a stateView scenario's `then`.
func (s Scenario) ResultThen() (ResultThen, error) {
	var out ResultThen
	if len(s.Then) == 0 {
		return out, fmt.Errorf("a stateView scenario needs a `then` describing the result")
	}
	if err := json.Unmarshal(s.Then, &out); err != nil {
		return out, fmt.Errorf("`then` is not a result expectation: %w", err)
	}
	return out, nil
}

// ---- the three namespaces ----
//
// There are three identifier spaces, and one of them is a PAIR:
//
//	schema id    ^[a-z0-9]+(-[a-z0-9]+)*$   "order-placed"
//	schema name  free prose                 "Order Placed"
//	pocketcqrs   an identifier              "OrderPlaced"
//
// Import folds two into one; export has to invent both from one. The
// resolution is a three-level precedence chain, resolved PER ELEMENT:
//
//	1. //@emid <id> <name> recorded in the generated source  (preferred)
//	2. a sidecar map shipped in the pack                     (secondary)
//	3. mechanical derivation                                 (the floor)
//
// The floor can never be removed, because export must also handle deciders
// that were never imported and so carry no recorded mapping at all. Both
// override layers are optional: a document imported without them still
// round-trips, just against the derivation rather than against equality.

// TypeName derives a pocketcqrs type from a schema element's name, falling
// back to its id when the name is unusable.
//
// "Order Placed" -> "OrderPlaced". Non-identifier characters are dropped
// rather than mangled, because the result has to be legal in a JS
// identifier, a file name and a collection name at once.
func TypeName(name, id string) string {
	if t := pascal(name); t != "" {
		return t
	}
	return pascal(strings.ReplaceAll(id, "-", " "))
}

// DeriveID turns a pocketcqrs type back into a schema id.
//
// The acronym case is the one that decides the rule: splitting purely on
// lower->upper boundaries turns "OrderPDFGenerated" into
// "order-p-d-f-generated". Splitting ALSO on an upper-run followed by
// upper+lower keeps runs intact: "order-pdf-generated".
func DeriveID(typeName string) string {
	words := splitWords(typeName)
	for i, w := range words {
		words[i] = strings.ToLower(w)
	}
	return strings.Join(words, "-")
}

// DeriveName turns a pocketcqrs type into a display name, using the same
// split as DeriveID so the two cannot disagree: "OrderPDFGenerated" ->
// "Order PDF Generated".
func DeriveName(typeName string) string {
	return strings.Join(splitWords(typeName), " ")
}

// splitWords breaks a PascalCase identifier into words, keeping acronym
// runs together.
func splitWords(s string) []string {
	runes := []rune(s)
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, string(cur))
			cur = nil
		}
	}
	for i, r := range runes {
		switch {
		case i == 0:
			cur = append(cur, r)
		case unicode.IsUpper(r) && !unicode.IsUpper(runes[i-1]):
			// lower -> upper: a new word starts here
			flush()
			cur = append(cur, r)
		case unicode.IsUpper(r) && unicode.IsUpper(runes[i-1]) &&
			i+1 < len(runes) && unicode.IsLower(runes[i+1]):
			// the last capital of a run belongs to the NEXT word:
			// "PDFGenerated" -> "PDF" + "Generated"
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return words
}

// pascal renders free prose as a PascalCase identifier, dropping anything
// that cannot appear in one.
func pascal(s string) string {
	var b strings.Builder
	upperNext := true
	for _, r := range s {
		switch {
		case unicode.IsLetter(r):
			if upperNext {
				b.WriteRune(unicode.ToUpper(r))
				upperNext = false
			} else {
				b.WriteRune(r)
			}
		case unicode.IsDigit(r):
			// a leading digit cannot start an identifier
			if b.Len() == 0 {
				continue
			}
			b.WriteRune(r)
			upperNext = false
		default:
			upperNext = true
		}
	}
	return b.String()
}

// LowerFirst renders a type as the lower-camel convention this project uses
// for aggregate names ("Order" -> "order", "SupportTicket" -> "supportTicket").
func LowerFirst(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

// ---- field types ----

// FoldType maps a schema fieldType onto one of this project's five
// //@schema types.
//
// Ten values fold onto five, so the mapping is deliberately lossy and
// deliberately documented. The two collapsing rules matter as much as the
// table: a LIST of anything and a field with SUBFIELDS both become json,
// because a column cannot hold either.
func FoldType(f Field) string {
	if f.Cardinality == "list" || len(f.Subfields) > 0 {
		return "json"
	}
	switch f.Type {
	case "string", "uuid":
		return "text"
	case "boolean":
		return "bool"
	case "integer", "long", "decimal", "double":
		return "number"
	case "date", "dateTime":
		return "date"
	case "custom":
		return "json"
	default:
		// an unknown type is not guessed at: text is the safe carrier, and
		// the caller reports the substitution rather than hiding it
		return "text"
	}
}

// FoldNote explains a fold that lost something, or "" when it was faithful.
// The caller puts these in the lint report — a fold nobody is told about is
// the silent wrong-doing this project keeps paying for.
func FoldNote(owner string, f Field) string {
	switch {
	case f.Cardinality == "list":
		return fmt.Sprintf("%s.%s is a list of %s and becomes a json column", owner, f.Name, f.Type)
	case len(f.Subfields) > 0:
		return fmt.Sprintf("%s.%s has subfields and becomes a json column", owner, f.Name)
	case f.Type == "uuid":
		return fmt.Sprintf("%s.%s is a uuid and becomes text (no uuid column type here)", owner, f.Name)
	case f.Type == "long" || f.Type == "decimal" || f.Type == "double" || f.Type == "integer":
		return fmt.Sprintf("%s.%s is %s and becomes number (one numeric column type here)", owner, f.Name, f.Type)
	case f.Type == "dateTime":
		return fmt.Sprintf("%s.%s is dateTime and becomes date", owner, f.Name)
	case f.Type == "custom":
		return fmt.Sprintf("%s.%s has a custom type and becomes a json column", owner, f.Name)
	case FoldType(f) == "text" && f.Type != "string":
		return fmt.Sprintf("%s.%s has unknown type %q and is carried as text", owner, f.Name, f.Type)
	}
	return ""
}
