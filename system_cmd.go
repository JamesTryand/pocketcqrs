package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/JamesTryand/pocketcqrs/events"
)

// newSystemCommand builds the `system` CLI command group.
// The app is bootstrapped when RunE executes, so the store is available.
func newSystemCommand(c *components) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "system",
		Short: "Inspect and control system-wide state",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "maintenance <on|off|status>",
		Short: "Get or set maintenance mode (domain commands are rejected while on)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			switch args[0] {
			case "status":
				mode, err := c.store.Mode(ctx)
				if err != nil {
					return err
				}
				cmd.Printf("mode: %s\n", mode)
				return nil
			case "on":
				if err := c.store.SetMode(ctx, events.ModeMaintenance); err != nil {
					return err
				}
				cmd.Println("maintenance mode ON: domain commands are rejected (503); schema-bearing functions may now be reloaded (POST /api/cqrs/admin/reload)")
				return nil
			case "off":
				if err := c.store.SetMode(ctx, events.ModeRunning); err != nil {
					return err
				}
				cmd.Println("maintenance mode OFF: serving domain commands")
				return nil
			default:
				return fmt.Errorf("unknown argument %q (want on|off|status)", args[0])
			}
		},
	})
	return cmd
}
