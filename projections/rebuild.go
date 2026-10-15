package projections

import (
	"context"
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/writeguard"
)

// rebuildTable holds rebuild requests in the node's own data.db: the node
// whose engine will act on them, never the replicated events.db.
const rebuildTable = "_cqrsRebuildRequests"

// RebuildRequests records "rebuild this projection" for the engine to
// consume (consumers.RebuildRequests). The CLI, the admin API and the
// dashboard only record; the running engine wipes, resets and replays
// between its passes, so it stays the only thing that runs a projection
// and no lock or "server running" flag is needed. One row per projection
// name: a repeated request is the same request.
type RebuildRequests struct {
	db dbx.Builder
}

// NewRebuildRequests opens the request table in db (the app's data.db),
// creating it if needed.
func NewRebuildRequests(db dbx.Builder) (*RebuildRequests, error) {
	if _, err := db.NewQuery(`CREATE TABLE IF NOT EXISTS ` + rebuildTable + ` (
		name TEXT PRIMARY KEY NOT NULL,
		requested TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%fZ'))
	)`).Execute(); err != nil {
		return nil, fmt.Errorf("projections: rebuild requests: %w", err)
	}
	return &RebuildRequests{db: db}, nil
}

// Request records a rebuild of name. Requesting it again before the engine
// has acted is harmless: it stays one request.
func (r *RebuildRequests) Request(ctx context.Context, name string) error {
	_, err := r.db.NewQuery(`INSERT OR IGNORE INTO ` + rebuildTable + ` (name) VALUES ({:name})`).
		WithContext(ctx).Bind(dbx.Params{"name": name}).Execute()
	return err
}

// Pending lists the projections with a rebuild requested, oldest first.
func (r *RebuildRequests) Pending(ctx context.Context) ([]string, error) {
	var names []string
	err := r.db.NewQuery(`SELECT name FROM ` + rebuildTable + ` ORDER BY requested, name`).
		WithContext(ctx).Column(&names)
	return names, err
}

// Done takes name's request off, once the engine has acted on it.
func (r *RebuildRequests) Done(ctx context.Context, name string) error {
	_, err := r.db.NewQuery(`DELETE FROM ` + rebuildTable + ` WHERE name = {:name}`).
		WithContext(ctx).Bind(dbx.Params{"name": name}).Execute()
	return err
}

var _ consumers.RebuildRequests = (*RebuildRequests)(nil)

// Wipe returns the engine's Wipe for app: it deletes every row of the
// projection's collections, as an internal write the write guard lets
// through. Only the engine calls it, between passes.
func Wipe(app core.App) func(ctx context.Context, c consumers.Consumer) error {
	return func(ctx context.Context, c consumers.Consumer) error {
		owner, ok := c.(interface{ Collections() []string })
		if !ok {
			return fmt.Errorf("projections: %s owns no collections to wipe", c.Name())
		}
		for _, name := range owner.Collections() {
			recs, err := app.FindRecordsByFilter(name, "", "", -1, 0)
			if err != nil {
				return fmt.Errorf("projections: listing %q: %w", name, err)
			}
			for _, rec := range recs {
				if err := app.DeleteWithContext(writeguard.MarkInternal(ctx), rec); err != nil {
					return fmt.Errorf("projections: deleting %q record %q: %w", name, rec.Id, err)
				}
			}
		}
		return nil
	}
}
