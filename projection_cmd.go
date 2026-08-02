package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"

	"pocketcqrs/consumers"
	"pocketcqrs/projections"
	"pocketcqrs/writeguard"
)

// allProjections is the single source of truth for the platform's
// projections, used by both the serve wiring and the projection CLI.
func allProjections(app core.App) []projections.Projection {
	return []projections.Projection{
		projections.NewTasks(app),
		projections.NewOrders(app),
	}
}

// newProjectionCommand builds the `projection` CLI command group.
// The app is already bootstrapped by the time RunE executes (see
// PocketBase.Execute), so components built in OnBootstrap are available.
func newProjectionCommand(c *components) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "projection",
		Short: "Manage CQRS projections",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "rebuild <name>",
		Short: "Wipe a projection's collections and rebuild them by replaying the event log",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			var target projections.Projection
			for _, p := range allProjections(c.app) {
				if p.Name() == name {
					target = p
					break
				}
			}
			if target == nil {
				return fmt.Errorf("unknown projection %q", name)
			}
			if c.store == nil {
				return errors.New("event store not initialized")
			}

			ctx := context.Background()

			// wipe the target collections (internal writes pass the writeguard)
			wiped := 0
			for _, colName := range target.Collections() {
				recs, err := c.app.FindRecordsByFilter(colName, "", "", -1, 0)
				if err != nil {
					return fmt.Errorf("failed listing %q records: %w", colName, err)
				}
				for _, rec := range recs {
					if err := c.app.DeleteWithContext(writeguard.MarkInternal(ctx), rec); err != nil {
						return fmt.Errorf("failed deleting %q record %q: %w", colName, rec.Id, err)
					}
					wiped++
				}
			}

			// reset the durable checkpoint and replay the log
			if err := c.store.SaveCheckpoint(ctx, target.Name(), 0); err != nil {
				return err
			}

			engine := consumers.NewEngine(c.store, nil)
			engine.Register(target)
			if err := engine.RunOnce(ctx); err != nil {
				return fmt.Errorf("replay failed: %w", err)
			}

			pos, err := c.store.Checkpoint(ctx, target.Name())
			if err != nil {
				return err
			}
			cmd.Printf("Projection %q rebuilt: wiped %d record(s), replayed to position %d.\n", name, wiped, pos)
			return nil
		},
	})
	return cmd
}
