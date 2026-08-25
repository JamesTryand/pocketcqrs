package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/jamestryand/pocketcqrs/packs"
)

// newEventsCommand builds the `events` CLI command group: export/import a
// pack's committed event history (slice/merge) — deliberately a separate
// verb from `pack export`/`pack import`, not a flag on them, so an ordinary
// dev->production promotion (which should never move event data) can't
// carry it by muscle memory. See events-db-slice-merge-scope.md.
func newEventsCommand(c *components, functionsDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Export and import a pack's committed event history (slice/merge, never promotion)",
	}

	export := &cobra.Command{
		Use:   "export <packdir>",
		Short: "Write events.ndjson for a pack's declared aggregates (manifest.json's Aggregates)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			packDir := args[0]
			manifest, err := packs.ReadManifest(packDir)
			if err != nil {
				return err
			}
			if len(manifest.Aggregates) == 0 {
				return fmt.Errorf("pack %q declares no Aggregates — set them with `pack export --aggregates` "+
					"before exporting its event data", manifest.Name)
			}

			outFile := filepath.Join(packDir, "events.ndjson")
			count, err := packs.ExportEvents(context.Background(), c.Store, manifest.Aggregates, outFile)
			if err != nil {
				return err
			}
			cmd.Printf("Exported %d event(s) across %d aggregate(s) to %s.\n",
				count, len(manifest.Aggregates), outFile)
			return nil
		},
	}
	cmd.AddCommand(export)

	imp := &cobra.Command{
		Use:   "import <packdir>",
		Short: "Bulk-insert a pack's events.ndjson, refusing on any stream/aggregate-name collision",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			packDir := args[0]
			dryRun, _ := cmd.Flags().GetBool("dry-run")

			manifest, err := packs.ReadManifest(packDir)
			if err != nil {
				return err
			}
			// Precondition (events-db-slice-merge-scope.md §4): the pack's
			// own code must already be imported and active, or a same-pack
			// reactor/effect function wouldn't exist in c.Engine.Names()
			// yet and would start from checkpoint 0 — exactly the bug this
			// whole mechanism exists to prevent, just for a same-pack
			// consumer instead of a pre-existing one.
			if err := requirePackCodeImported(manifest, *functionsDir); err != nil {
				return err
			}

			evtsFile := filepath.Join(packDir, "events.ndjson")
			result, err := packs.ImportEvents(context.Background(), c.Store, c.Store, c.Engine.Names(), evtsFile, dryRun)
			if err != nil {
				return err
			}

			banner := ""
			if result.DryRun {
				banner = " (dry run, nothing written)"
			}
			cmd.Printf("Imported %d event(s) across %d stream(s)%s.\n", result.Imported, len(result.Streams), banner)
			for _, s := range result.Streams {
				cmd.Printf("  %s\n", s)
			}
			if len(result.AdvancedCheckpoints) > 0 {
				cmd.Printf("Effect-tier checkpoints advanced past the imported batch%s:\n", banner)
				names := make([]string, 0, len(result.AdvancedCheckpoints))
				for n := range result.AdvancedCheckpoints {
					names = append(names, n)
				}
				sort.Strings(names)
				for _, n := range names {
					cmd.Printf("  %s -> %d\n", n, result.AdvancedCheckpoints[n])
				}
			}
			return nil
		},
	}
	imp.Flags().Bool("dry-run", false, "preview the import (collision check + would-be checkpoint advances) without writing")
	cmd.AddCommand(imp)

	return cmd
}

// requirePackCodeImported checks (a cheap file-existence check, not a
// reload) that every function file manifest declares is already present in
// functionsDir, refusing with a pointed error naming the first one missing
// otherwise. See newEventsCommand's import RunE for why this must be
// enforced, not just documented.
func requirePackCodeImported(manifest *packs.Manifest, functionsDir string) error {
	for _, name := range manifest.Functions {
		if _, err := os.Stat(filepath.Join(functionsDir, name)); err != nil {
			return fmt.Errorf("pack %q's code is not imported yet (%s missing from %s) — "+
				"import the pack's code and reload before importing its event data",
				manifest.Name, name, functionsDir)
		}
	}
	return nil
}
