package authverify

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pocketbase/pocketbase/core"
)

// Materialize turns a verdict into a live *core.Record bound to this
// node's own collection of the same name — the shape everything downstream
// of re.Auth expects (gateway.actorMeta reads .Id and .Collection().Name;
// apis.RequireAuth checks the bound collection). Collections exist
// identically on every node (they come from code migrations, run per
// node); it is only the record ROW that doesn't, which is exactly what the
// verdict carries. Resolved by name, not id: nothing guarantees a
// migration-created collection gets the same id on every node.
//
// Fields hidden from the master's serialization (PublicExport) are absent
// from the materialized record — a documented limitation for API rules
// that reference hidden fields, not silently different data.
func Materialize(app core.App, v *Verdict) (*core.Record, error) {
	col, err := app.FindCachedCollectionByNameOrId(v.CollectionName)
	if err != nil {
		return nil, fmt.Errorf("authverify: no local collection %q for verified record: %w", v.CollectionName, err)
	}
	if !col.IsAuth() {
		return nil, fmt.Errorf("authverify: local collection %q is not an auth collection", v.CollectionName)
	}

	var fields map[string]any
	if err := json.Unmarshal(v.Record, &fields); err != nil {
		return nil, fmt.Errorf("authverify: malformed verdict record: %w", err)
	}
	// serialization helpers, not record fields
	delete(fields, "collectionId")
	delete(fields, "collectionName")
	delete(fields, "expand")

	rec := core.NewRecord(col)
	rec.Load(fields)
	if rec.Id == "" {
		return nil, errors.New("authverify: verdict record has no id")
	}
	return rec, nil
}
