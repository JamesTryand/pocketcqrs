package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jamestryand/pocketcqrs/emschema"
	"github.com/jamestryand/pocketcqrs/scaffold"
)

// newSchemaImportCommand builds the standalone `schema import` command.
//
// Extracted so main() can run it directly, before PocketBase ever
// bootstraps (see main()'s short-circuit, mirroring the one already there
// for `skill`) — import is a pure file-to-file transformation (emschema.Load
// / Map / Verify, then plain os.WriteFile calls) with no dependency on a
// running platform: it touches no *components field and no core.App. Making
// it go through app.Start() anyway was F-10 — PocketBase bootstraps
// (creating a full pb_data/) before any RunE runs and offers no per-command
// opt-out, so even a REFUSED import left a data directory behind. It also
// enabled F-9 for this command specifically: PocketBase's own
// pb.Execute() discards RootCmd.Execute()'s return value ("leave to the
// commands to decide whether to print their error" — pocketbase.go), so a
// RunE error was printed but never turned into a non-zero exit code. Running
// this command's own Execute() directly, outside that wrapper, fixes both:
// no bootstrap happens at all, and main() can act on the returned error.
//
// It is still ALSO registered under the ordinary `schema` group by
// newSchemaCommand below, purely so `pocketcqrs --help`/`schema --help`
// continue to list it — mirroring the `skill` command's own precedent
// (main.go's short-circuit comment explains why that copy stays registered
// too). A real `schema import` invocation never reaches that copy.
func newSchemaImportCommand() *cobra.Command {
	imp := &cobra.Command{
		Use:   "import <document.json|manifest-dir>",
		Short: "Map an EventModeling document onto generated function files",
		Long: "Reads a single document or a split manifest directory, maps it onto this\n" +
			"project's domain model, and writes the generated slice into a directory.\n\n" +
			"Nothing is activated: review the files, then save them through the editor\n" +
			"or copy them into the functions directory and reload behind the barrier.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			outDir, _ := cmd.Flags().GetString("out")
			docsDir, _ := cmd.Flags().GetString("docs")
			overrides, _ := cmd.Flags().GetStringSlice("aggregate")
			force, _ := cmd.Flags().GetBool("force")
			skipScenarios, _ := cmd.Flags().GetBool("skip-scenarios")
			functionsDir, _ := cmd.Flags().GetString("functionsDir")
			lang, _ := cmd.Flags().GetString("lang")
			if lang != "js" && lang != "go" {
				return fmt.Errorf("--lang wants \"js\" or \"go\", got %q", lang)
			}

			opts := emschema.Options{AggregateOverrides: map[string]string{}}
			for _, kv := range overrides {
				id, name, ok := strings.Cut(kv, "=")
				if !ok || id == "" || name == "" {
					return fmt.Errorf("--aggregate wants <elementId>=<aggregateName>, got %q", kv)
				}
				opts.AggregateOverrides[id] = name
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

			// Run the document's own scenarios against the code just
			// generated, before anything is written. A scenario is a
			// given/when/then, which is exactly a dry run — so a document can
			// say whether the slice it produced behaves as the model claims,
			// at the one moment that is cheap to act on.
			if !skipScenarios {
				scratch, err := os.MkdirTemp("", "pcqrs-scenarios-")
				if err != nil {
					return err
				}
				defer os.RemoveAll(scratch)
				results, err := emschema.Verify(doc, mapped, scratch)
				if err != nil {
					return err
				}
				printScenarios(cmd, results, lang)
			}

			if outDir == "" {
				cmd.Println("\nNo --out directory given, so nothing was written. " +
					"The mapping above is what an import would produce.")
				return nil
			}

			if lang == "go" {
				written, err := writeSliceGo(mapped, outDir, force)
				if err != nil {
					return err
				}
				cmd.Printf("\nWrote %d file(s) to %s\n", written, outDir)
				if docsDir != "" {
					cmd.Printf("--docs %s was ignored: domain docs are generated only for --lang js.\n", docsDir)
				}
				cmd.Println("\nScaffolding, not migration: there is no JS→Go transpiler. Each file is a " +
					"starting-point skeleton (state shape, Decide/Evolve signatures, Commands metadata) " +
					"whose actual rules are yours to write — dry-run and test it independently before " +
					"relying on it.")
				printGoWiringSuggestions(cmd, mapped.Domains)
				return nil
			}

			written, err := writeSlice(mapped, outDir, docsDir, force)
			if err != nil {
				return err
			}
			cmd.Printf("\nWrote %d file(s) to %s\n", written, outDir)
			cmd.Println("Nothing is live yet. Save each file through the editor (or copy it into " +
				functionsDir + "), then reload — schema-bearing files need maintenance mode first.")
			return nil
		},
	}
	imp.Flags().String("out", "", "write the generated function files into this directory")
	imp.Flags().String("docs", "", "write per-aggregate domain docs into this directory (e.g. docs/domains); ignored with --lang go")
	imp.Flags().StringSlice("aggregate", nil,
		"map an untagged element to an aggregate: --aggregate notify-shipping-partner=shipmentNotice (repeatable)")
	imp.Flags().Bool("force", false, "overwrite existing files")
	imp.Flags().Bool("skip-scenarios", false,
		"do not run the document's scenarios against the generated code")
	imp.Flags().String("lang", "js", "output language: \"js\" (hot-reloadable functions, the default) or \"go\" (compiled source, CLI-only — see docs/go-guide.md)")
	// Its own local flag, not shared with the app's --functionsDir: this
	// command never runs alongside a booted app, so there's nothing to
	// share it with. Only used for the completion message above.
	imp.Flags().String("functionsDir", "pb_functions",
		"the directory the completion message tells you to copy generated files into")
	return imp
}

// newSchemaCommand builds the `schema` CLI group: import and export
// EventModeling documents (github.com/jamestryand/eventmodelschema).
//
// Import is ONE-SHOT and writes nothing live. It produces the same files the
// dashboard's scaffolder produces, saved through the ordinary function-file
// path, so imported code goes through the same load check, dry run and
// maintenance barrier as anything hand-written. There is no new activation
// machinery, deliberately: a document is a description, and describing
// something must not be a way to bypass the gates.
func newSchemaCommand(c *components) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Import and export EventModeling documents",
	}

	cmd.AddCommand(newSchemaImportCommand())

	exp := &cobra.Command{
		Use:   "export <document.json>",
		Short: "Render the running platform's catalog as an EventModeling document",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cat, err := c.buildCatalog(cmd.Context())
			if err != nil {
				return err
			}
			doc, report := emschema.FromCatalog(cat)
			raw, err := emschema.Marshal(doc)
			if err != nil {
				return err
			}
			if err := os.WriteFile(args[0], raw, 0o644); err != nil {
				return err
			}
			printReport(cmd, report)
			cmd.Printf("\nWrote %s\n", args[0])
			return nil
		},
	}
	cmd.AddCommand(exp)

	return cmd
}

// printReport prints what the mapping decided, could not decide, and could
// not carry.
//
// This is the whole point of the report existing: every choice taken on the
// document's behalf is named here, or it is a silent wrong-doing. Printing
// counts alone would be the same failure in smaller print.
func printReport(cmd *cobra.Command, rep *emschema.Report) {
	if rep == nil {
		return
	}
	if len(rep.Errors) > 0 {
		cmd.Printf("REFUSED (%d):\n", len(rep.Errors))
		for _, e := range rep.Errors {
			cmd.Printf("  ✗ %s\n", e)
		}
	}
	if len(rep.Warnings) > 0 {
		cmd.Printf("Decisions taken on the document's behalf (%d):\n", len(rep.Warnings))
		for _, w := range rep.Warnings {
			cmd.Printf("  ! %s\n", w)
		}
	}
	if len(rep.Lossy) > 0 {
		cmd.Printf("Not carried across (%d):\n", len(rep.Lossy))
		for _, l := range rep.Lossy {
			cmd.Printf("  – %s\n", l)
		}
	}
	if len(rep.Errors)+len(rep.Warnings)+len(rep.Lossy) == 0 {
		cmd.Println("Mapped with nothing lost and nothing assumed.")
	}
}

// printScenarios reports what the document's own examples say about the code
// just generated.
//
// A failing scenario is NOT an error. The generated slice is a starting point
// whose rules are the author's job, so a scenario failing usually means the
// document describes behaviour nobody has written yet — which is worth
// knowing precisely, and worth not treating as a broken import.
func printScenarios(cmd *cobra.Command, results []emschema.ScenarioResult, lang string) {
	// Verify always runs the scenarios against the JS mapping (dryrun's
	// engine has no Go counterpart) regardless of --lang: for --lang go this
	// checks the same model the Go files were generated from, not the Go
	// files themselves. Say so, rather than letting "the generated code"
	// read as a claim that the Go output was exercised.
	against := "the generated code"
	if lang == "go" {
		against = "the JS mapping of the same model (no Go-specific scenario checker exists yet)"
	}
	if len(results) == 0 {
		cmd.Printf("\nThe document declares no scenarios, so nothing could be checked "+
			"against %s.\n", against)
		return
	}
	var passed, failed, skipped int
	for _, r := range results {
		switch {
		case r.Skipped:
			skipped++
		case r.Passed:
			passed++
		default:
			failed++
		}
	}
	cmd.Printf("\nScenarios checked against %s: %d passed, %d failed, %d skipped\n",
		against, passed, failed, skipped)
	for _, r := range results {
		mark := "✗"
		switch {
		case r.Skipped:
			mark = "–"
		case r.Passed:
			mark = "✓"
		}
		cmd.Printf("  %s [%s] %s\n      %s\n", mark, r.Kind, r.Name, r.Detail)
	}
	if failed > 0 {
		cmd.Println("\nA failing scenario is not a broken import: the generated slice records what " +
			"happened and refuses the obvious contradictions, and every other rule is yours to write. " +
			"These are the rules the document says are still missing.")
	}
}

// writeSlice writes the generated files and the domain docs.
//
// An existing file is kept unless --force. That matters most for the domain
// docs: they are meant to be edited after import, and a re-import silently
// overwriting someone's prose would be the worst kind of helpfulness. It is
// the same rule catalog.WriteSkeletons follows, so the two generators of
// docs/domains/*.md cannot fight.
func writeSlice(mapped *emschema.Mapped, outDir, docsDir string, force bool) (int, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return 0, err
	}
	written := 0
	for _, d := range mapped.Domains {
		files, err := d.Generate()
		if err != nil {
			return written, fmt.Errorf("generating %s: %w", d.Aggregate, err)
		}
		for _, f := range files {
			n, err := writeIfAbsent(filepath.Join(outDir, f.Name), f.Source, force)
			if err != nil {
				return written, err
			}
			written += n
		}
	}
	if docsDir == "" {
		return written, nil
	}
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		return written, err
	}
	for agg, md := range mapped.DomainDocs {
		n, err := writeIfAbsent(filepath.Join(docsDir, agg+".md"), md, force)
		if err != nil {
			return written, err
		}
		written += n
	}
	return written, nil
}

// writeSliceGo writes the generated Go files. No docsDir counterpart:
// --lang go is CLI + --out only (docs/go-guide.md's own JS→Go table is the
// standing documentation for what converting means), and domain docs are a
// JS-side artifact of the dashboard scaffolder's own workflow.
func writeSliceGo(mapped *emschema.Mapped, outDir string, force bool) (int, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return 0, err
	}
	written := 0
	for _, d := range mapped.Domains {
		files, err := d.GenerateGo()
		if err != nil {
			return written, fmt.Errorf("generating %s: %w", d.Aggregate, err)
		}
		for _, f := range files {
			n, err := writeIfAbsent(filepath.Join(outDir, f.Name), f.Source, force)
			if err != nil {
				return written, err
			}
			written += n
		}
	}
	return written, nil
}

// printGoWiringSuggestions prints the registration line(s) a real deployment
// needs to actually run the generated code, as suggested text — never
// applied automatically. Auto-patching main.go for a one-line edit is not
// worth the code-mod machinery it would take, and there is deliberately no
// "graduated" bookkeeping: the registry already refuses a JS/Go name
// collision, so a partial migration is caught immediately rather than
// silently (docs/go-guide.md's "A word of caution on scope").
func printGoWiringSuggestions(cmd *cobra.Command, domains []scaffold.Domain) {
	cmd.Println("\nSuggested wiring (not applied automatically):")
	for _, d := range domains {
		// scaffold.ExportName and scaffold.GoPackageName, not a re-derived
		// copy of the same casing transform: this line must name the exact
		// package/func GenerateGo's files declare, or the suggestion could
		// silently drift from the code it's describing.
		pkg := scaffold.GoPackageName(d.Aggregate)
		aggregateCtor := scaffold.ExportName(d.Aggregate)
		cmd.Printf("\n  decider.Register(registry, %q, %s.%s())\n", d.Aggregate, pkg, aggregateCtor)
		for _, rm := range d.ReadModels {
			ctor := "New" + scaffold.ExportName(rm.Collection)
			cmd.Printf("  // read model %q needs a PocketBase migration for its collection --\n", rm.Collection)
			cmd.Println("  // see migrations/1754200000_tasks_collection.go for the shape.")
			cmd.Printf("  engine.Register(%s.%s(app))\n", pkg, ctor)
		}
		for _, r := range d.Reactors {
			reactorCtor := scaffold.ExportName(r.Name)
			cmd.Printf("  engine.Register(reactors.AsConsumer(%s.%s(), registry, logger, warn))\n", pkg, reactorCtor)
		}
	}
}

func writeIfAbsent(path, content string, force bool) (int, error) {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return 0, nil
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return 0, err
	}
	return 1, nil
}
