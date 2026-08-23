package functions

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
)

// FieldSpec is one declared projection field.
type FieldSpec struct {
	Name string
	Type string // text | number | bool | date | json | relation
	// RelationTarget names the related collection when Type is "relation".
	RelationTarget string
}

// SchemaSpec is the declared output schema of a JS projection.
type SchemaSpec struct {
	Collection string
	Fields     []FieldSpec
	Key        string // field used for idempotent upserts (unique index)

	// RuleOverride is this collection's //@rule value, if the file declared
	// one; nil means no directive, so createCollection falls back to the
	// deployment-wide --cqrsSchemaDefaultRule. Distinct from "" (which would
	// be indistinguishable from "not set") -- a directive of "public" must
	// still win over a deployment default of "authenticated".
	RuleOverride *string
}

// schemaFieldTypes maps directive types to PocketBase field constructors.
// "relation" is deliberately absent: a relation field needs its target
// collection's id, resolved during the second reconcile pass.
var schemaFieldTypes = map[string]func(name string) core.Field{
	"text":   func(n string) core.Field { return &core.TextField{Name: n} },
	"number": func(n string) core.Field { return &core.NumberField{Name: n} },
	"bool":   func(n string) core.Field { return &core.BoolField{Name: n} },
	"date":   func(n string) core.Field { return &core.DateField{Name: n} },
	"json":   func(n string) core.Field { return &core.JSONField{Name: n} },
}

// relationTypePattern matches relation(<collection>) type strings; the
// capture group is already restricted to validName-conformant names.
var relationTypePattern = regexp.MustCompile(`^relation\(([a-zA-Z][a-zA-Z0-9_]*)\)$`)

// validName restricts collection/field names: they are interpolated into
// index DDL, so anything beyond a simple identifier is rejected outright.
var validName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

// parseSchemaDirective parses "<collection> <field>:<type> ..." and the
// accompanying key field.
func parseSchemaDirective(rest string, key string) (*SchemaSpec, error) {
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return nil, fmt.Errorf("schema directive needs a collection and at least one field, got %q", rest)
	}
	spec := &SchemaSpec{Collection: fields[0], Key: key}
	if !validName.MatchString(spec.Collection) {
		return nil, fmt.Errorf("invalid collection name %q", spec.Collection)
	}
	keySeen := false
	for _, f := range fields[1:] {
		name, typ, ok := strings.Cut(f, ":")
		if !ok || !validName.MatchString(name) {
			return nil, fmt.Errorf("invalid field spec %q (want name:type)", f)
		}
		field := FieldSpec{Name: name, Type: typ}
		if _, ok := schemaFieldTypes[typ]; !ok {
			m := relationTypePattern.FindStringSubmatch(typ)
			if m == nil {
				return nil, fmt.Errorf("invalid field type %q in %q (want text|number|bool|date|json|relation(<collection>))", typ, f)
			}
			field.Type = "relation"
			field.RelationTarget = m[1]
		}
		if name == key {
			keySeen = true
		}
		spec.Fields = append(spec.Fields, field)
	}
	if key == "" {
		return nil, fmt.Errorf("schema for %s is missing its //@key directive", spec.Collection)
	}
	if !validName.MatchString(key) {
		return nil, fmt.Errorf("invalid key field name %q", key)
	}
	if !keySeen {
		return nil, fmt.Errorf("key field %q is not declared in the schema", key)
	}
	return spec, nil
}

// resolveRuleValue turns a //@rule or --cqrsSchemaDefaultRule value into the
// PocketBase rule expression createCollection sets on ListRule/ViewRule.
// "" and "public" both mean no restriction (PocketBase's own meaning of an
// empty rule -- so an unset flag reproduces the pre-Item-9 default exactly);
// "authenticated" requires a non-anonymous request; anything else is used
// verbatim as a raw PocketBase rule expression, the escape hatch the
// decision doc asked for.
func resolveRuleValue(value string) string {
	switch value {
	case "", "public":
		return ""
	case "authenticated":
		return `@request.auth.id != ""`
	default:
		return value
	}
}

// uniqueIndexSQL builds the key index DDL. Names are regex-validated above.
func (s *SchemaSpec) uniqueIndexSQL() string {
	return fmt.Sprintf("CREATE UNIQUE INDEX idx_%s_%s ON %s (%s)", s.Collection, s.Key, s.Collection, s.Key)
}

// ReconcileSchemas materializes declared schemas into PocketBase collections
// at boot time (a restart IS the maintenance window).
//
// Two passes over every schema of every spec: the first creates/extends
// collections with their non-relation fields, the second adds missing
// relation fields and the key indexes. Relation targets may be collections
// declared by other specs (in any order) or pre-existing ones (e.g.
// migration-created or auth collections), so no relation is wired before
// every declared collection exists.
//
// Reconciliation is additive-only, mirroring the append-only event rule:
// missing collections are created, missing fields and the key index are
// added — existing fields are never removed, retyped, or renamed (a
// declared/actual type mismatch is logged and kept as-is).
// defaultRule is the resolved value of --cqrsSchemaDefaultRule: "" (or
// "public") reproduces the pre-Item-9 default, "authenticated" locks every
// newly created //@schema collection down, and anything else is a raw
// PocketBase rule expression. A collection's own //@rule directive
// (SchemaSpec.RuleOverride) wins over this when set.
func ReconcileSchemas(app core.App, specs []*ProjectionSpec, defaultRule string) error {
	for _, spec := range specs {
		for _, s := range spec.Schemas {
			if err := reconcileBase(app, spec, s, defaultRule); err != nil {
				return err
			}
		}
	}
	for _, spec := range specs {
		for _, s := range spec.Schemas {
			if err := reconcileRelations(app, spec, s); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcileBase is pass 1: the collection exists and carries all declared
// non-relation fields.
func reconcileBase(app core.App, spec *ProjectionSpec, s *SchemaSpec, defaultRule string) error {
	col, err := app.FindCollectionByNameOrId(s.Collection)
	if err != nil {
		return createCollection(app, s, defaultRule)
	}

	changed := false
	for _, f := range s.Fields {
		if f.Type == "relation" {
			continue // pass 2
		}
		existing := col.Fields.GetByName(f.Name)
		if existing == nil {
			col.Fields.Add(schemaFieldTypes[f.Type](f.Name))
			changed = true
			continue
		}
		if existing.Type() != f.Type {
			spec.runtime.logger("schema field type mismatch, keeping existing",
				"collection", s.Collection, "field", f.Name,
				"declared", f.Type, "actual", existing.Type())
		}
	}

	if !changed {
		return nil
	}
	return app.Save(col)
}

// reconcileRelations is pass 2: missing relation fields are added (the
// target collection id is resolved by name — every declared collection
// exists by now) along with the key index.
func reconcileRelations(app core.App, spec *ProjectionSpec, s *SchemaSpec) error {
	col, err := app.FindCollectionByNameOrId(s.Collection)
	if err != nil {
		return fmt.Errorf("schema reconcile: collection %s missing after base pass: %w", s.Collection, err)
	}

	changed := false
	for _, f := range s.Fields {
		if f.Type != "relation" {
			continue
		}
		if existing := col.Fields.GetByName(f.Name); existing != nil {
			if existing.Type() != "relation" {
				spec.runtime.logger("schema field type mismatch, keeping existing",
					"collection", s.Collection, "field", f.Name,
					"declared", f.Type, "actual", existing.Type())
			}
			continue
		}
		target, err := app.FindCollectionByNameOrId(f.RelationTarget)
		if err != nil {
			return fmt.Errorf("schema %s: relation field %s: target collection %q not found: %w",
				s.Collection, f.Name, f.RelationTarget, err)
		}
		col.Fields.Add(&core.RelationField{Name: f.Name, CollectionId: target.Id, MaxSelect: 1})
		changed = true
	}

	idx := s.uniqueIndexSQL()
	found := false
	for _, existing := range col.Indexes {
		if existing == idx {
			found = true
			break
		}
	}
	if !found {
		col.Indexes = append(col.Indexes, idx)
		changed = true
	}

	if !changed {
		return nil
	}
	return app.Save(col)
}

func createCollection(app core.App, s *SchemaSpec, defaultRule string) error {
	col := core.NewBaseCollection(s.Collection)
	// read side: writes are rejected by the writeguard regardless of this
	// rule. Precedence: the collection's own //@rule, else the
	// deployment-wide --cqrsSchemaDefaultRule, else public (unchanged
	// pre-Item-9 default). Never reached again once the collection exists
	// -- reconcileBase only calls this for a brand-new collection, so an
	// existing collection's rule is never touched by a later reload.
	rule := defaultRule
	if s.RuleOverride != nil {
		rule = *s.RuleOverride
	}
	expr := resolveRuleValue(rule)
	col.ListRule = types.Pointer(expr)
	col.ViewRule = types.Pointer(expr)
	for _, f := range s.Fields {
		if f.Type == "relation" {
			continue // pass 2: the target collection may not exist yet
		}
		field := schemaFieldTypes[f.Type](f.Name)
		if f.Name == s.Key {
			if tf, ok := field.(*core.TextField); ok {
				tf.Required = true
			}
		}
		col.Fields.Add(field)
	}
	return app.Save(col)
}
