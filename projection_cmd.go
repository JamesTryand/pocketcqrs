package main

import (
	"context"

	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"

	"github.com/jamestryand/pocketcqrs/projections"
)

// allProjections is the single source of truth for the platform's Go
// projections, used by both the serve wiring and the projection CLI.
//
// The platform ships none of its own: the Go projections in this repo are
// example content, and exist only under --tutorial. It is a method rather
// than a function taking a bool because every call site already holds the
// components, which makes calling it with the wrong answer impossible.
func (c *components) allProjections(app core.App) []projections.Projection {
	if !c.Tutorial {
		return nil
	}
	return exampleProjections(app)
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
		Short: "Request a projection rebuild: the server's engine wipes its collections and replays the log",
		Long: "Records a rebuild request in this node's data.db. The running server's engine carries it out\n" +
			"between its passes (wipe the projection's collections, reset its checkpoint, replay the log), so\n" +
			"it stays the only thing that runs the projection. If no server is running, the rebuild happens\n" +
			"when one starts. The read model is empty or partial until the replay catches up. Works on a\n" +
			"secondary too (its checkpoints and data.db are its own). Requesting it again is harmless.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := c.RequestRebuild(context.Background(), name); err != nil {
				return err
			}
			cmd.Printf("Rebuild of projection %q requested. The server's engine wipes and replays it on its next pass "+
				"(at once if it is running, otherwise when it starts); its read model is partial until then.\n", name)
			return nil
		},
	})
	return cmd
}
