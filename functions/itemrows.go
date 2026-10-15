package functions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/projections/itemrows"
)

// itemRowsOp is rowsPerListItem's op (schema 3.16.0): a row per group of a
// stream's list items, planned by package itemrows. Streams names the
// projection's companion collection, keyed by stream, that holds each
// stream's itemrows.State; the host reads that back, never the projection,
// so a fixture verify run answers the same as the live one (the reason
// increment exists, see rowOp).
type itemRowsOp struct {
	itemrows.Op
	Streams string
}

func parseItemRows(v any) (*itemRowsOp, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("itemRows must be {stream, streams, listField, groupBy, ...}, got %T", v)
	}
	op := &itemRowsOp{Op: itemrows.Op{Sums: map[string]string{}, Fields: map[string]any{}}}
	op.Stream = fmt.Sprint(m["stream"])
	op.Streams, _ = m["streams"].(string)
	op.ListField, _ = m["listField"].(string)
	op.GroupBy, _ = m["groupBy"].(string)
	if m["stream"] == nil || op.Stream == "" || op.Streams == "" || op.ListField == "" || op.GroupBy == "" {
		return nil, fmt.Errorf("itemRows needs stream, streams, listField and groupBy")
	}
	if sums, ok := m["sums"].(map[string]any); ok {
		for field, sub := range sums {
			s, ok := sub.(string)
			if !ok {
				return nil, fmt.Errorf("itemRows sum %q must name a subfield", field)
			}
			op.Sums[field] = s
		}
	}
	if fields, ok := m["fields"].(map[string]any); ok {
		op.Fields = fields
	}
	if items, ok := m["items"]; ok {
		op.HasItems = true
		list, ok := items.([]any)
		if !ok && items != nil {
			return nil, fmt.Errorf("itemRows items must be a list, got %T", items)
		}
		op.Items = list
	}
	op.Remove, _ = m["remove"].(bool)
	return op, nil
}

// applyItemRows applies op live: the stream's state from its companion row,
// then the rows it plans.
func (spec *ProjectionSpec) applyItemRows(ctx context.Context, app core.App, s *SchemaSpec, op *itemRowsOp) error {
	streams, err := spec.schemaNamed(op.Streams)
	if err != nil {
		return err
	}
	stateRec, err := app.FindFirstRecordByData(streams.Collection, streams.Key, op.Stream)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var prior itemrows.State
	if stateRec != nil {
		if err := stateRec.UnmarshalJSONField("state", &prior); err != nil {
			return fmt.Errorf("itemRows: %s state of %s: %w", streams.Collection, op.Stream, err)
		}
	}
	next, drop, rows := op.Plan(prior)
	for _, key := range drop {
		if err := spec.applyOp(ctx, app, s, rowOp{delete: true, key: key}); err != nil {
			return err
		}
	}
	for _, r := range rows {
		if err := spec.applyOp(ctx, app, s, rowOp{key: r.Key, fields: r.Fields}); err != nil {
			return err
		}
	}
	if next == nil {
		return spec.applyOp(ctx, app, streams, rowOp{delete: true, key: op.Stream})
	}
	return spec.applyOp(ctx, app, streams, rowOp{key: op.Stream, fields: map[string]any{"state": next}})
}

// simulateItemRows is applyItemRows over a dry run's rows.
func (spec *ProjectionSpec) simulateItemRows(out *ProjectionDryRun, s *SchemaSpec, op *itemRowsOp) error {
	streams, err := spec.schemaNamed(op.Streams)
	if err != nil {
		return err
	}
	table := func(name string) map[string]map[string]any {
		if out.Rows[name] == nil {
			out.Rows[name] = map[string]map[string]any{}
		}
		return out.Rows[name]
	}
	var prior itemrows.State
	if row := table(streams.Collection)[op.Stream]; row != nil {
		raw, _ := json.Marshal(row["state"])
		_ = json.Unmarshal(raw, &prior)
	}
	next, drop, rows := op.Plan(prior)
	for _, key := range drop {
		out.Deletes++
		delete(table(s.Collection), key)
	}
	for _, r := range rows {
		out.Upserts++
		row := map[string]any{s.Key: r.Key}
		for k, v := range r.Fields {
			if !reservedRowFields[k] && k != s.Key {
				row[k] = v
			}
		}
		table(s.Collection)[r.Key] = row
	}
	if next == nil {
		delete(table(streams.Collection), op.Stream)
		return nil
	}
	table(streams.Collection)[op.Stream] = map[string]any{streams.Key: op.Stream, "state": next}
	return nil
}

// schemaNamed is the projection's declared schema for collection name.
func (spec *ProjectionSpec) schemaNamed(name string) (*SchemaSpec, error) {
	for _, s := range spec.Schemas {
		if s.Collection == name {
			return s, nil
		}
	}
	return nil, fmt.Errorf("itemRows: the projection declares no %q collection for its streams", name)
}
