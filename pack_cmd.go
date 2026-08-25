package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jamestryand/pocketcqrs/packs"
	"github.com/jamestryand/pocketcqrs/projections"
)

// newPackCommand builds the `pack` CLI command group: export/import domain
// packs. The app is bootstrapped when RunE executes.
func newPackCommand(c *components, functionsDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pack",
		Short: "Export and import domain packs (pb_functions + collection schemas + manifest)",
	}

	export := &cobra.Command{
		Use:   "export <outdir>",
		Short: "Bundle function files (and optionally plain collections) as a domain pack",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _ := cmd.Flags().GetString("name")
			version, _ := cmd.Flags().GetString("version")
			description, _ := cmd.Flags().GetString("description")
			fns, _ := cmd.Flags().GetStringSlice("functions")
			cols, _ := cmd.Flags().GetStringSlice("collections")

			guarded := projections.GuardedCollections(c.allProjections(c.App)...)
			for _, p := range c.JSProjs {
				guarded = append(guarded, p.Collections()...)
			}

			manifest, err := packs.Export(c.App, *functionsDir, args[0], packs.ExportOptions{
				Name:               name,
				Version:            version,
				Description:        description,
				Functions:          fns,
				Collections:        cols,
				GuardedCollections: guarded,
			})
			if err != nil {
				return err
			}
			cmd.Printf("Exported pack %q v%s to %s: %d function file(s), %d collection schema(s).\n",
				manifest.Name, manifest.Version, args[0], len(manifest.Functions), len(manifest.Collections))
			return nil
		},
	}
	export.Flags().String("name", "", "pack name (required)")
	export.Flags().String("version", "", "pack version (default 0.1.0)")
	export.Flags().String("description", "", "pack description")
	export.Flags().StringSlice("functions", nil, "function files to include (default: all .js in the functions dir)")
	export.Flags().StringSlice("collections", nil, "plain collections to include (projection-owned are refused)")
	_ = export.MarkFlagRequired("name")
	cmd.AddCommand(export)

	imp := &cobra.Command{
		Use:   "import <packdir>",
		Short: "Install a domain pack (function files + collection schemas)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			result, err := packs.Import(c.App, *functionsDir, args[0], force)
			if err != nil {
				return err
			}
			m := result.Manifest
			cmd.Printf("Imported pack %q v%s: %d function file(s) copied, %d skipped (existing), %d collection schema(s) imported.\n",
				m.Name, m.Version, len(result.FunctionsCopied), len(result.FunctionsSkipped), result.CollectionsImported)
			for _, name := range result.FunctionsSkipped {
				cmd.Printf("  skipped %s (exists; --force to overwrite)\n", name)
			}
			if len(result.FunctionsCopied) > 0 {
				fmt.Println("Next: restart, or `system maintenance on` + POST /api/cqrs/admin/reload + `system maintenance off`.")
				fmt.Println("Tip: dry-run first — `dryrun decider`/`dryrun projection` against the pack's files.")
			}
			return nil
		},
	}
	imp.Flags().Bool("force", false, "overwrite existing function files")
	cmd.AddCommand(imp)

	return cmd
}
