package main

import (
	"context"
	"net/http"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"

	"pocketcqrs/catalog"
)

// buildCatalog collects the catalog from the live components (shared by
// the CLI and the JSON endpoint).
func (c *components) buildCatalog(ctx context.Context) (*catalog.Catalog, error) {
	return catalog.Build(ctx, catalog.Inputs{
		App:           c.app,
		Store:         c.store,
		Registry:      c.registry,
		Engine:        c.engine,
		Runtime:       c.fnRuntime,
		HTTP:          c.httpFns,
		GoProjections: allProjections(c.app),
		JSProjs:       c.jsProjs,
		JSDeciders:    c.jsDeciders,
	})
}

// newCatalogCommand builds the `catalog` CLI command: print the platform
// catalog as Markdown (+ Mermaid) or JSON, or generate domain-doc skeletons.
// The app is bootstrapped when RunE executes.
func newCatalogCommand(c *components) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Introspect the platform: aggregates, events, consumers, collections, flows",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat, err := c.buildCatalog(cmd.Context())
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

// registerCatalogRoute binds the superuser-only catalog endpoint:
//
//	GET /api/cqrs/catalog
func registerCatalogRoute(e *core.ServeEvent, c *components) {
	e.Router.GET("/api/cqrs/catalog", func(re *core.RequestEvent) error {
		cat, err := c.buildCatalog(re.Request.Context())
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		return re.JSON(http.StatusOK, cat)
	}).Bind(apis.RequireSuperuserAuth())
}
