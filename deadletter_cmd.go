package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/JamesTryand/pocketcqrs/events"
)

// newDeadletterCommand builds the `deadletter` CLI command group:
// inspect and retry failed function deliveries. The app is bootstrapped by
// the time RunE executes, so the function runtime (with the current code)
// and the event store are available in components.
func newDeadletterCommand(c *components) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deadletter",
		Short: "Inspect and retry dead-lettered function deliveries",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List pending dead letters (--all to include resolved)",
		RunE: func(cmd *cobra.Command, args []string) error {
			includeResolved, _ := cmd.Flags().GetBool("all")
			letters, err := c.store.DeadLetters(context.Background(), includeResolved)
			if err != nil {
				return err
			}
			if len(letters) == 0 {
				cmd.Println("No dead letters.")
				return nil
			}
			for _, dl := range letters {
				status := "pending"
				if dl.Resolved {
					status = "resolved"
				}
				cmd.Printf("#%d [%s] %s on %s %s/%s (event pos %d, attempts %d, last %s)\n  error: %s\n",
					dl.ID, status, dl.Consumer, dl.Event.Type, dl.Event.Aggregate, dl.Event.AggregateID,
					dl.EventPos, dl.Attempts, dl.LastFailed, dl.Error)
			}
			return nil
		},
	}
	list.Flags().Bool("all", false, "include resolved dead letters")
	cmd.AddCommand(list)

	retry := &cobra.Command{
		Use:   "retry <id|all>",
		Short: "Re-deliver a dead letter through the current function code",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			letters, err := c.store.DeadLetters(ctx, false)
			if err != nil {
				return err
			}
			if args[0] != "all" {
				id, err := strconv.ParseInt(args[0], 10, 64)
				if err != nil {
					return fmt.Errorf("invalid id %q (or use \"all\")", args[0])
				}
				letters = filterLetters(letters, id)
				if len(letters) == 0 {
					return fmt.Errorf("no pending dead letter with id %d", id)
				}
			}
			if len(letters) == 0 {
				cmd.Println("No pending dead letters.")
				return nil
			}

			// same adjudication as POST /api/cqrs/deadletters/{id}/retry —
			// shared so the CLI and the dashboard can never diverge
			results, err := c.retryDeadLetters(ctx, letters)
			if err != nil {
				return err
			}
			for _, res := range results {
				if res.Resolved {
					cmd.Printf("#%d retried successfully, resolved.\n", res.ID)
				} else {
					cmd.Printf("#%d retry failed (attempt %d): %v\n", res.ID, res.Attempts, res.Error)
				}
			}
			return nil
		},
	}
	cmd.AddCommand(retry)

	cmd.AddCommand(&cobra.Command{
		Use:   "dismiss <id>",
		Short: "Mark a dead letter resolved without retrying",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid id %q", args[0])
			}
			if err := c.store.ResolveDeadLetter(context.Background(), id); err != nil {
				return fmt.Errorf("no dead letter with id %d", id)
			}
			cmd.Printf("#%d dismissed.\n", id)
			return nil
		},
	})

	return cmd
}

func filterLetters(letters []events.DeadLetter, id int64) []events.DeadLetter {
	var out []events.DeadLetter
	for _, dl := range letters {
		if dl.ID == id {
			out = append(out, dl)
		}
	}
	return out
}
