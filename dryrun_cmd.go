package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/functions"
)

// newDryrunCommand builds the `dryrun` CLI command group: run candidate JS
// code against real history without appending events or touching
// collections. The app is bootstrapped when RunE executes.
func newDryrunCommand(c *components) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dryrun",
		Short: "Run candidate JS code against real history without persisting anything",
	}

	// -----------------------------------------------------------------
	extract := &cobra.Command{
		Use:   "extract <aggregate> <aggregate-id>",
		Short: "Extract a stream as a JSON fixture (the harness's 'prior events')",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			stream, err := c.store.LoadStream(context.Background(), args[0], args[1])
			if err != nil {
				return err
			}
			if len(stream) == 0 {
				return fmt.Errorf("no events for %s/%s", args[0], args[1])
			}
			out, err := json.MarshalIndent(stream, "", "  ")
			if err != nil {
				return err
			}
			file, _ := cmd.Flags().GetString("file")
			if file != "" {
				if err := os.WriteFile(file, out, 0o644); err != nil {
					return err
				}
				cmd.Printf("Wrote %d event(s) to %s.\n", len(stream), file)
				return nil
			}
			cmd.Println(string(out))
			return nil
		},
	}
	extract.Flags().String("file", "", "write the fixture to a file instead of stdout")
	cmd.AddCommand(extract)

	// -----------------------------------------------------------------
	deciderCmd := &cobra.Command{
		Use:   "decider <file.js> [aggregate-id]",
		Short: "Fold existing streams through a candidate JS decider (no appends)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := functions.LoadDeciderFile(c.fnRuntime, args[0])
			if err != nil {
				return err
			}
			streamID := ""
			if len(args) == 2 {
				streamID = args[1]
			}
			res, err := functions.DryRunDecider(c.store, spec, streamID)
			if err != nil {
				return err
			}
			cmd.Printf("OK: folded %d stream(s), %d event(s) of aggregate %q.\n",
				res.Streams, res.Events, res.Aggregate)
			if streamID != "" {
				state, _ := json.MarshalIndent(res.State, "", "  ")
				cmd.Printf("Final state of %s: %s\n", streamID, state)
			}
			return nil
		},
	}
	cmd.AddCommand(deciderCmd)

	// -----------------------------------------------------------------
	decideCmd := &cobra.Command{
		Use:   "decide <file.js> <aggregate-id> <command> [payload-json]",
		Short: "Show the events a command WOULD produce on a real stream (no append)",
		Args:  cobra.RangeArgs(3, 4),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := functions.LoadDeciderFile(c.fnRuntime, args[0])
			if err != nil {
				return err
			}
			payload := json.RawMessage(`{}`)
			if len(args) == 4 && args[3] != "" {
				if !json.Valid([]byte(args[3])) {
					return fmt.Errorf("payload is not valid JSON")
				}
				payload = json.RawMessage(args[3])
			}
			meta := map[string]any{
				"now":   time.Now().UTC().Format("2006-01-02 15:04:05.000Z"),
				"actor": "dryrun",
			}
			produced, err := functions.DryRunDecide(c.store, spec, args[1],
				decider.Command{Name: args[2], Payload: payload}, meta)
			if err != nil {
				return err
			}
			if len(produced) == 0 {
				cmd.Println("The decider returned NO events.")
				return nil
			}
			out, _ := json.MarshalIndent(produced, "", "  ")
			cmd.Printf("The decider WOULD produce %d event(s) (nothing appended):\n%s\n", len(produced), out)
			return nil
		},
	}
	cmd.AddCommand(decideCmd)

	// -----------------------------------------------------------------
	projCmd := &cobra.Command{
		Use:   "projection <file.js>",
		Short: "Simulate a candidate JS projection over the log in memory (--diff against the live collection)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := functions.LoadProjectionFile(c.fnRuntime, c.app, args[0])
			if err != nil {
				return err
			}
			res, err := functions.DryRunProjection(c.store, spec)
			if err != nil {
				return err
			}
			totalRows := 0
			for _, rows := range res.Rows {
				totalRows += len(rows)
			}
			cmd.Printf("Simulated %q over %d event(s): %d upsert(s), %d delete(s), %d final row(s) across %d collection(s).\n",
				res.Name, res.Events, res.Upserts, res.Deletes, totalRows, len(spec.Schemas))

			diff, _ := cmd.Flags().GetBool("diff")
			if !diff {
				return nil
			}
			totalDiffs := 0
			for _, s := range spec.Schemas {
				live, err := c.app.FindRecordsByFilter(s.Collection, "", "", -1, 0)
				if err != nil {
					return err
				}
				liveRows := make([]map[string]any, 0, len(live))
				for _, rec := range live {
					liveRows = append(liveRows, rec.PublicExport())
				}
				diffs := functions.DiffRows(res.Rows[s.Collection], liveRows, s.Key)
				if len(diffs) == 0 {
					cmd.Printf("No differences in %s: simulation matches the live collection.\n", s.Collection)
					continue
				}
				totalDiffs += len(diffs)
				cmd.Printf("%d difference(s) in %s vs the live collection:\n", len(diffs), s.Collection)
				for _, d := range diffs {
					cmd.Println("  " + d)
				}
			}
			if totalDiffs == 0 && len(spec.Schemas) > 1 {
				cmd.Println("All collections match.")
			}
			return nil
		},
	}
	projCmd.Flags().Bool("diff", false, "compare the simulation against the live collection")
	cmd.AddCommand(projCmd)

	return cmd
}
