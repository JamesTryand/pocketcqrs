package emschema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// The importer fails closed. The source schema says so twice (3.8.0 for
// access rules, 3.10.0 for refusals): a generator that cannot apply a
// declaration must refuse to generate, never drop it. A plain json.Unmarshal
// drops every key the Go types don't name, so a document using a newer
// feature (a read model's selfAccess, a command's refuseWhen, a removal)
// would import into code that quietly lacks it.
//
// So every key in the document is checked against the types in document.go,
// as an allow-list: a key is accepted only if this package reads it. That
// covers features nobody here has heard of yet, too (anything after the
// vendored schema), not only the ones listed in laterFeatures, which exist
// only to make the message say what was refused. This is dotnetcqrs's
// UnsupportedFeatures.cs turned round: it lists what it refuses, which suits
// a generator that is nearly complete; this importer is many releases
// behind, so it lists what it accepts.
//
// As a feature is implemented, its field is added to the Go types and it
// leaves the refused set by itself.

// UnsupportedFeature is one place in a document that declares something this
// importer cannot apply.
type UnsupportedFeature struct {
	// Pointer is a JSON pointer into the document. In a split document it
	// is prefixed with the file it is in, relative to the manifest.
	Pointer string
	// What says what the feature is, and the schema version that added it
	// when known.
	What string
}

// UnsupportedError refuses a whole document, naming every unsupported
// feature in it, so one import shows everything there is to do.
type UnsupportedError struct {
	Source   string
	Features []UnsupportedFeature
}

func (e *UnsupportedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "emschema: %s declares %d thing(s) pocketcqrs cannot apply yet, so nothing was generated "+
		"(dropping them would generate code without them):", e.Source, len(e.Features))
	for _, f := range e.Features {
		fmt.Fprintf(&b, "\n  %s: %s", f.Pointer, f.What)
	}
	return b.String()
}

// laterFeatures names the keys added to the schema after the generation this
// importer applies, for the message only. Refusal does not depend on this
// table: an unknown key is refused whether or not it is listed here.
var laterFeatures = map[string]string{
	"selfAccess":        "selfAccess, a caller seeing only their own rows (schema 3.2.0; param 3.8.0)",
	"dataSubjects":      "dataSubjects.erasure, who may request erasure (schema 3.2.0)",
	"outcomes":          "outcomes, a command's alternative results (schema 3.3.0; when and set 3.12.0, repeated 3.13.0)",
	"ingresses":         "an ingress (schema 3.4.0)",
	"ingressId":         "a slice started by an ingress (schema 3.4.0)",
	"delay":             "an automation started by a delay (schema 3.5.0)",
	"schedule":          "an automation started by a schedule (schema 3.5.0)",
	"elapsed":           "an elapsed step in a scenario (schema 3.5.0)",
	"effect":            "an automation effect (schema 3.6.0)",
	"partitioning":      "partitioning (schema 3.7.0)",
	"partitioned":       "partitioned, opting out of partitioning (schema 3.7.0)",
	"partitionFrom":     "partitionFrom (schema 3.7.0)",
	"grantsAccess":      "a scope that grants access (schema 3.8.0)",
	"caller":            "a scenario's caller (schema 3.8.0)",
	"removedByEventIds": "removedByEventIds, events that remove a read model's row (schema 3.9.0)",
	"refuseWhen":        "refuseWhen, rules that refuse a command (schema 3.10.0; numeric operators 3.11.0)",
	"actor":             "a scenario's actor (schema 3.10.0)",
	"at":                "a scenario's time (schema 3.10.0)",
	"rowKeyField":       "a read model's rowKeyField (schema 3.14.0)",
	"stateField":        "requiredOwnership.stateField, ownership decided from the aggregate's own state (schema 3.15.0)",
	"message":           "requiredOwnership.message (schema 3.15.0)",
	"rowsPerListItem":   "rowsPerListItem, a row per group of a stream's list items (schema 3.16.0)",
	"subfield":          "a sumOfItems derivation (schema 3.16.0)",
	"values":            "a byEvent derivation (schema 3.16.0)",
	"projection":        "projection: a read model built by a hand-written projection (schema 3.17.0)",
}

// laterKinds names the enum values added later, for the message only.
var laterKinds = map[string]string{
	"sumOfItems": "derivation kind sumOfItems (schema 3.16.0)",
	"byEvent":    "derivation kind byEvent (schema 3.16.0)",
}

var (
	derivationKinds = map[string]bool{DerivationToggle: true, DerivationCount: true, DerivationSum: true, DerivationGroupBy: true}
	filterKinds     = map[string]bool{FilterDateRange: true, FilterMatch: true}
	rawMessageType  = reflect.TypeOf(json.RawMessage{})
)

// supportChecker walks raw JSON beside the Go type it decodes into.
type supportChecker struct {
	found []UnsupportedFeature
}

// check walks raw against into's type, prefixing every pointer with prefix.
// A document that isn't valid JSON is left for json.Unmarshal to report.
func (c *supportChecker) check(raw []byte, into reflect.Type, prefix string) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return
	}
	c.walk(v, into, prefix)
}

func (c *supportChecker) refuseKey(at, key string) {
	what, ok := laterFeatures[key]
	if !ok {
		what = fmt.Sprintf("%q, which this importer does not read (a schema feature newer than %s?)", key, SchemaVersion)
	}
	c.found = append(c.found, UnsupportedFeature{Pointer: at, What: what})
}

func (c *supportChecker) refuseKind(at, kind string) {
	what, ok := laterKinds[kind]
	if !ok {
		what = fmt.Sprintf("kind %q, which this importer does not know", kind)
	}
	c.found = append(c.found, UnsupportedFeature{Pointer: at, What: what})
}

func (c *supportChecker) walk(v any, t reflect.Type, at string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	// raw JSON is either example data (any key is fine) or a kind-dependent
	// shape a hook below checks; an interface is a literal value
	if t == rawMessageType || t.Kind() == reflect.Interface {
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := v.(map[string]any)
		if !ok {
			return
		}
		fields := jsonFields(t)
		for _, k := range sortedKeys(obj) {
			p := at + "/" + pointerEscape(k)
			f, ok := fields[k]
			if !ok {
				c.refuseKey(p, k)
				continue
			}
			c.walk(obj[k], f, p)
		}
		c.shapeHooks(t, obj, at)
	case reflect.Slice:
		// RoleList is a slice that also accepts a bare string; anything
		// that isn't an array is left to json.Unmarshal
		arr, ok := v.([]any)
		if !ok {
			return
		}
		for i, e := range arr {
			c.walk(e, t.Elem(), fmt.Sprintf("%s/%d", at, i))
		}
	case reflect.Map:
		obj, ok := v.(map[string]any)
		if !ok {
			return
		}
		for _, k := range sortedKeys(obj) {
			c.walk(obj[k], t.Elem(), at+"/"+pointerEscape(k))
		}
	}
}

// shapeHooks checks what the Go types alone can't: kind-dependent raw
// shapes, and enum values added after this importer's generation.
func (c *supportChecker) shapeHooks(t reflect.Type, obj map[string]any, at string) {
	kind, _ := obj["kind"].(string)
	switch t {
	case reflect.TypeOf(Scenario{}):
		var when, then reflect.Type
		switch kind {
		case KindStateChange:
			when, then = reflect.TypeOf(CommandRef{}), reflect.TypeOf(EventsThen{})
		case KindError:
			when, then = reflect.TypeOf(CommandRef{}), reflect.TypeOf(ErrorThen{})
		case KindStateView:
			when, then = reflect.TypeOf(ReadModelQuery{}), reflect.TypeOf(ResultThen{})
		default:
			return // lint refuses an unknown scenario kind
		}
		if w, ok := obj["when"]; ok {
			c.walk(w, when, at+"/when")
		}
		if th, ok := obj["then"]; ok {
			c.walk(th, then, at+"/then")
		}
	case reflect.TypeOf(FieldDerivation{}):
		if !derivationKinds[kind] {
			c.refuseKind(at+"/kind", kind)
		}
	case reflect.TypeOf(ReadModelFilter{}):
		if !filterKinds[kind] {
			c.refuseKind(at+"/kind", kind)
		}
	}
}

// jsonFields maps each JSON key a struct reads to its field's type.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

// pointerEscape escapes a key for a JSON pointer (RFC 6901).
func pointerEscape(k string) string {
	return strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
}

// refuseUnsupported returns an *UnsupportedError for whatever c found, or nil.
func (c *supportChecker) refuseUnsupported(source string) error {
	if len(c.found) == 0 {
		return nil
	}
	return &UnsupportedError{Source: source, Features: c.found}
}
