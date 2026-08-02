package functions

import (
	"github.com/pocketbase/pocketbase/core"
)

// Reader is the controlled, read-only access functions get to the query
// side. There is deliberately no write path: state changes must go through
// the command API so they become events (the writeguard enforces this).
type Reader interface {
	// FindRecord returns one record by id (PublicExport shape), or nil.
	FindRecord(collection, id string) (map[string]any, error)
	// Query returns up to limit records matching a PocketBase filter.
	Query(collection, filter string, limit int) ([]map[string]any, error)
}

// AppReader implements Reader against the PocketBase app.
type AppReader struct {
	App core.App
}

// NewAppReader creates a Reader backed by app.
func NewAppReader(app core.App) *AppReader { return &AppReader{App: app} }

// FindRecord implements Reader.
func (r *AppReader) FindRecord(collection, id string) (map[string]any, error) {
	rec, err := r.App.FindRecordById(collection, id)
	if err != nil {
		return nil, nil // not found
	}
	return rec.PublicExport(), nil
}

// Query implements Reader.
func (r *AppReader) Query(collection, filter string, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	recs, err := r.App.FindRecordsByFilter(collection, filter, "", limit, 0)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec.PublicExport())
	}
	return out, nil
}
