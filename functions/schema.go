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
	Type string // text | number | bool | date | json
}

// SchemaSpec is the declared output schema of a JS projection.
type SchemaSpec struct {
	Collection string
	Fields     []FieldSpec
	Key        string // field used for idempotent upserts (unique index)
}

// schemaFieldTypes maps directive types to PocketBase field constructors.
var schemaFieldTypes = map[string]func(name string) core.Field{
	"text":   func(n string) core.Field { return &core.TextField{Name: n} },
	"number": func(n string) core.Field { return &core.NumberField{Name: n} },
	"bool":   func(n string) core.Field { return &core.BoolField{Name: n} },
	"date":   func(n string) core.Field { return &core.DateField{Name: n} },
	"json":   func(n string) core.Field { return &core.JSONField{Name: n} },
}

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
		if _, ok := schemaFieldTypes[typ]; !ok {
			return nil, fmt.Errorf("invalid field type %q in %q (want text|number|bool|date|json)", typ, f)
		}
		if name == key {
			keySeen = true
		}
		spec.Fields = append(spec.Fields, FieldSpec{Name: name, Type: typ})
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

// uniqueIndexSQL builds the key index DDL. Names are regex-validated above.
func (s *SchemaSpec) uniqueIndexSQL() string {
	return fmt.Sprintf("CREATE UNIQUE INDEX idx_%s_%s ON %s (%s)", s.Collection, s.Key, s.Collection, s.Key)
}

// ReconcileSchemas materializes declared schemas into PocketBase collections
// at boot time (a restart IS the maintenance window).
//
// Reconciliation is additive-only, mirroring the append-only event rule:
// missing collections are created, missing fields and the key index are
// added — existing fields are never removed, retyped, or renamed (a
// declared/actual type mismatch is logged and kept as-is).
func ReconcileSchemas(app core.App, specs []*ProjectionSpec) error {
	for _, spec := range specs {
		if err := reconcileOne(app, spec); err != nil {
			return err
		}
	}
	return nil
}

func reconcileOne(app core.App, spec *ProjectionSpec) error {
	s := spec.Schema
	col, err := app.FindCollectionByNameOrId(s.Collection)
	if err != nil {
		return createCollection(app, s)
	}

	changed := false
	for _, f := range s.Fields {
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

func createCollection(app core.App, s *SchemaSpec) error {
	col := core.NewBaseCollection(s.Collection)
	// read side: publicly queryable; writes rejected by the writeguard
	col.ListRule = types.Pointer("")
	col.ViewRule = types.Pointer("")
	for _, f := range s.Fields {
		field := schemaFieldTypes[f.Type](f.Name)
		if f.Name == s.Key {
			if tf, ok := field.(*core.TextField); ok {
				tf.Required = true
			}
		}
		col.Fields.Add(field)
	}
	col.Indexes = types.JSONArray[string]{s.uniqueIndexSQL()}
	return app.Save(col)
}
