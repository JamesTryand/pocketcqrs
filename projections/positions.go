package projections

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/consumers"
)

// positionsTable holds each projection's position in the node's own data.db,
// beside its rows, so the two commit together. Never the replicated
// events.db: that is the master's single-writer log, read-only elsewhere.
const positionsTable = "_cqrsPositions"

func ensurePositions(db dbx.Builder) error {
	if _, err := db.NewQuery(`CREATE TABLE IF NOT EXISTS ` + positionsTable + ` (
		name TEXT PRIMARY KEY NOT NULL,
		position INTEGER NOT NULL
	)`).Execute(); err != nil {
		return fmt.Errorf("projections: positions: %w", err)
	}
	return nil
}

// Positions is a projection's checkpoint store in data.db (a
// consumers.CheckpointStore for OwnCheckpoints). A projection with no
// position yet reads consumers.ErrNoCheckpoint, so the engine carries on
// from the one it kept before.
type Positions struct {
	App core.App
}

func (p Positions) Checkpoint(ctx context.Context, name string) (int64, error) {
	if err := ensurePositions(p.App.DB()); err != nil {
		return 0, err
	}
	var pos int64
	err := p.App.DB().NewQuery(`SELECT position FROM ` + positionsTable + ` WHERE name = {:name}`).
		WithContext(ctx).Bind(dbx.Params{"name": name}).Row(&pos)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, consumers.ErrNoCheckpoint
	}
	return pos, err
}

// SaveCheckpoint records a position on its own (a rebuild's reset to 0);
// an event's position is recorded by Commit, with its rows.
func (p Positions) SaveCheckpoint(ctx context.Context, name string, position int64) error {
	if err := ensurePositions(p.App.DB()); err != nil {
		return err
	}
	return savePosition(ctx, p.App.DB(), name, position)
}

func savePosition(ctx context.Context, db dbx.Builder, name string, position int64) error {
	_, err := db.NewQuery(`INSERT INTO ` + positionsTable + ` (name, position) VALUES ({:name}, {:position})
		ON CONFLICT(name) DO UPDATE SET position = excluded.position`).
		WithContext(ctx).Bind(dbx.Params{"name": name, "position": position}).Execute()
	return err
}

// Commit runs apply, an event's row ops, and records position as name's in
// one data.db transaction: either both land or neither does, so a
// crash or a failing op can't leave half an event applied, or its rows
// without its position (applied twice on the next pass). apply must write
// through the tx app it is given.
func Commit(ctx context.Context, app core.App, name string, position int64, apply func(tx core.App) error) error {
	if err := ensurePositions(app.DB()); err != nil {
		return err
	}
	return app.RunInTransaction(func(tx core.App) error {
		if err := apply(tx); err != nil {
			return err
		}
		return savePosition(ctx, tx.DB(), name, position)
	})
}
