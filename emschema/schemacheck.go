package emschema

import (
	"bytes"
	_ "embed"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/dlclark/regexp2/v2"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// The importer refuses a document the schema refuses. The allow-list in
// supported.go fails closed on keys this package does not read, but a key
// the schema requires and the document leaves out (a view scenario's
// caller.subjectId, required since 3.8.0) used to import cleanly, and
// generated code from a document that was not valid. dotnetcqrs's generator
// validates against its copy of the schema; this does the same, so the two
// agree on what a valid document is.
//
// The schema is the vendored copy in schema/ (go:embed cannot reach the
// repository's testdata/; a test checks the two copies are identical). It
// is registered under its own $id, so nothing is ever fetched.

//go:embed schema/eventmodeling.schema.json
var embeddedSchema []byte

var (
	compiledSchema    *jsonschema.Schema
	compiledSchemaErr error
	compileSchemaOnce sync.Once
)

func documentSchema() (*jsonschema.Schema, error) {
	compileSchemaOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(embeddedSchema))
		if err != nil {
			compiledSchemaErr = fmt.Errorf("emschema: the embedded schema is not JSON: %w", err)
			return
		}
		id, _ := doc.(map[string]any)["$id"].(string)
		c := jsonschema.NewCompiler()
		// the schema's patterns are ECMAScript (a duration's uses a
		// lookahead), which Go's regexp does not support
		c.UseRegexpEngine(ecmaRegexp)
		if err := c.AddResource(id, doc); err != nil {
			compiledSchemaErr = fmt.Errorf("emschema: the embedded schema: %w", err)
			return
		}
		compiledSchema, compiledSchemaErr = c.Compile(id)
	})
	return compiledSchema, compiledSchemaErr
}

// SchemaProblem is one place a document breaks the schema.
type SchemaProblem struct {
	// Pointer is a JSON pointer into the document. In a split document it
	// is prefixed with the file it is in, relative to the manifest.
	Pointer string
	Message string
}

// SchemaError refuses a whole document, naming every problem in it.
type SchemaError struct {
	Source   string
	Problems []SchemaProblem
}

func (e *SchemaError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "emschema: %s is not a valid EventModeling document (schema %s): %d problem(s), so nothing was generated:",
		e.Source, SchemaVersion, len(e.Problems))
	for _, p := range e.Problems {
		fmt.Fprintf(&b, "\n  %s: %s", p.Pointer, p.Message)
	}
	return b.String()
}

// validateDocument checks raw (one whole document) against the schema.
// locate, when not nil, rewrites a pointer into the joined document to the
// file it came from (a split document).
func validateDocument(source string, raw []byte, locate func(string) string) error {
	sch, err := documentSchema()
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("emschema: %s is not valid JSON: %w", source, err)
	}
	verr := sch.Validate(inst)
	if verr == nil {
		return nil
	}
	ve, ok := verr.(*jsonschema.ValidationError)
	if !ok {
		return fmt.Errorf("emschema: %s: %w", source, verr)
	}
	problems := schemaProblems(ve)
	if locate != nil {
		for i := range problems {
			problems[i].Pointer = locate(problems[i].Pointer)
		}
	}
	return &SchemaError{Source: source, Problems: problems}
}

var schemaPrinter = message.NewPrinter(language.English)

// schemaProblems reduces a validation error tree to the problems a person
// would fix. A oneOf or anyOf that nothing matched reports every
// alternative's failures, nearly all from alternatives the document never
// meant (a field that is not a derivation, a target that is not an
// ordering); only the alternative that got furthest into the document is
// kept, the one it was evidently written as. If that leaves nothing, every
// leaf is reported, so a refusal always says why.
//
// A "false schema" (unevaluatedProperties: false) beside a real problem is
// that problem's echo: an object whose subschema failed has none of its
// properties counted as evaluated, so every one of them is reported too. A
// false schema is dropped when a real problem lies inside the same object.
func schemaProblems(ve *jsonschema.ValidationError) []SchemaProblem {
	leaves := relevantLeaves(ve)
	if len(leaves) == 0 {
		leaves = allLeaves(ve)
	}
	leaves = withoutEchoes(leaves)
	seen := map[string]bool{}
	var out []SchemaProblem
	for _, l := range leaves {
		p := SchemaProblem{Pointer: instancePointer(l.InstanceLocation), Message: l.ErrorKind.LocalizedString(schemaPrinter)}
		key := p.Pointer + "\x00" + p.Message
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Pointer < out[j].Pointer })
	return out
}

func relevantLeaves(e *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(e.Causes) == 0 {
		return []*jsonschema.ValidationError{e}
	}
	switch e.ErrorKind.(type) {
	case *kind.OneOf, *kind.AnyOf:
		// the alternative whose failures reach deepest into the document
		var best []*jsonschema.ValidationError
		bestDepth := -1
		for _, alt := range e.Causes {
			leaves := relevantLeaves(alt)
			d := 0
			for _, l := range leaves {
				if len(l.InstanceLocation) > d {
					d = len(l.InstanceLocation)
				}
			}
			if d > bestDepth {
				best, bestDepth = leaves, d
			}
		}
		return best
	}
	var out []*jsonschema.ValidationError
	for _, c := range e.Causes {
		out = append(out, relevantLeaves(c)...)
	}
	return out
}

func withoutEchoes(leaves []*jsonschema.ValidationError) []*jsonschema.ValidationError {
	var real []string
	for _, l := range leaves {
		if _, echo := l.ErrorKind.(*kind.FalseSchema); !echo {
			real = append(real, instancePointer(l.InstanceLocation))
		}
	}
	var out []*jsonschema.ValidationError
	for _, l := range leaves {
		if _, echo := l.ErrorKind.(*kind.FalseSchema); echo && len(l.InstanceLocation) > 0 {
			parent := instancePointer(l.InstanceLocation[:len(l.InstanceLocation)-1])
			inside := false
			for _, r := range real {
				// on the object itself (a missing property) or inside it
				if r == parent || strings.HasPrefix(r, parent+"/") {
					inside = true
					break
				}
			}
			if inside {
				continue
			}
		}
		out = append(out, l)
	}
	return out
}

func allLeaves(e *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(e.Causes) == 0 {
		return []*jsonschema.ValidationError{e}
	}
	var out []*jsonschema.ValidationError
	for _, c := range e.Causes {
		out = append(out, allLeaves(c)...)
	}
	return out
}

func instancePointer(loc []string) string {
	var b strings.Builder
	for _, s := range loc {
		b.WriteString("/")
		b.WriteString(pointerEscape(s))
	}
	return b.String()
}

type ecmaPattern regexp2.Regexp

func (re *ecmaPattern) MatchString(s string) bool {
	ok, err := (*regexp2.Regexp)(re).MatchString(s)
	return err == nil && ok
}

func (re *ecmaPattern) String() string { return (*regexp2.Regexp)(re).String() }

func ecmaRegexp(s string) (jsonschema.Regexp, error) {
	re, err := regexp2.Compile(s, regexp2.ECMAScript)
	if err != nil {
		return nil, err
	}
	return (*ecmaPattern)(re), nil
}
