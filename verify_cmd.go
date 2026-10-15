package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/jamestryand/pocketcqrs/emschema"
)

// newSchemaVerifyCommand builds the standalone `schema verify` command: a
// document's scenarios run against the code they describe, with nothing
// written. Against the generated code by default; with --functions, against
// a host's own functions directory, so its hooks really decide and a hand
// projection really builds its rows (dotnetcqrs's host check).
//
// Like `schema import` it is a pure file transformation with no running
// platform, so main() runs it before PocketBase bootstraps (F-10), and a
// failing scenario is a non-zero exit (F-9): this is the check a build runs.
// HAND (a hook only the host can decide) is not a failure.
func newSchemaVerifyCommand() *cobra.Command {
	v := &cobra.Command{
		Use:   "verify <document.json|manifest-dir>",
		Short: "Run an EventModeling document's scenarios against generated or host code",
		Long: "Maps the document as `schema import` does and runs every scenario, writing\n" +
			"nothing. With --functions, the scenarios run against that directory's own\n" +
			"deciders, hand files and projections (a host's pb_functions) in place of the\n" +
			"generated ones. Exits non-zero when a scenario fails.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := mapOptions(cmd)
			if err != nil {
				return err
			}
			host, _ := cmd.Flags().GetString("functions")
			lang, _ := cmd.Flags().GetString("lang")
			checkout, _ := cmd.Flags().GetString("pocketcqrs")
			if lang != "js" && lang != "go" {
				return fmt.Errorf("--lang wants \"js\" or \"go\", got %q", lang)
			}
			doc, err := emschema.Load(args[0])
			if err != nil {
				return err
			}
			mapped, mapErr := emschema.Map(doc, opts)
			printReport(cmd, mapped.Report)
			if mapErr != nil {
				return mapErr
			}
			scratch, err := os.MkdirTemp("", "pcqrs-scenarios-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(scratch)
			results, err := emschema.VerifyWith(doc, mapped, scratch, emschema.VerifyOptions{Host: host, Lang: lang, Pocketcqrs: checkout})
			if err != nil {
				return err
			}
			against := "the generated code"
			if host != "" {
				against = "the host's functions in " + host
			}
			if lang == "go" {
				against = "the Go code (" + against[4:] + ")"
			}
			if failed := printScenarios(cmd, results, against); failed > 0 {
				return fmt.Errorf("%d scenario(s) failed", failed)
			}
			return nil
		},
	}
	addMapFlags(v)
	v.Flags().String("functions", "",
		"a functions directory (a host's pb_functions; with --lang go, a directory of its Go packages) whose own files the scenarios run against, by name; empty: the generated code")
	v.Flags().String("lang", "js",
		"\"js\": the JS dry run; \"go\": the Go code, compiled with a harness in a scratch module and run against PocketBase")
	v.Flags().String("pocketcqrs", "",
		"with --lang go, a pocketcqrs checkout the harness builds against; empty: the version this binary was built from")
	return v
}
