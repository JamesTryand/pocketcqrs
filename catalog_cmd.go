package main

import (
	"github.com/spf13/cobra"
)

// newCatalogCommand builds the `catalog` CLI command: print the platform
// catalog as Markdown (+ Mermaid) or JSON, or generate domain-doc skeletons.
// The app is bootstrapped when RunE executes.
func newCatalogCommand(c *components) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Introspect the platform: aggregates, events, consumers, collections, flows",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat, err := c.BuildCatalog(cmd.Context())
			if err != nil {
				return err
			}

			skeletonsDir, _ := cmd.Flags().GetString("skeletons")
			if skeletonsDir != "" {
				force, _ := cmd.Flags().GetBool("force")
				written, skipped, err := cat.WriteSkeletons(skeletonsDir, force)
				if err != nil {
					return err
				}
				for _, p := range written {
					cmd.Printf("wrote %s\n", p)
				}
				for _, p := range skipped {
					cmd.Printf("skipped %s (exists; --force to overwrite)\n", p)
				}
				if len(written) == 0 && len(skipped) == 0 {
					cmd.Println("no aggregates registered; nothing to generate")
				}
				return nil
			}

			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				out, err := cat.JSON()
				if err != nil {
					return err
				}
				cmd.Println(string(out))
				return nil
			}

			cmd.Print(cat.Markdown())
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "print the catalog as JSON instead of Markdown")
	cmd.Flags().String("skeletons", "", "generate domain-doc skeletons into this directory (e.g. docs/domains)")
	cmd.Flags().Bool("force", false, "overwrite existing skeleton files")
	return cmd
}
