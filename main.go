// Command pocketcqrs is the stock program: the host package's Run with the
// `skill` command, which lives here because its files are embedded from this
// directory. An embedder with compiled-in Go domains writes a main.go like
// this one with its own host.Options (docs/go-guide.md, "Your own program").
package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/jamestryand/pocketcqrs/host"
)

func main() {
	// `skill` copies files out of the binary and touches nothing else, so run
	// it before PocketBase exists.
	//
	// Going through app.Start() would BOOTSTRAP THE WHOLE APPLICATION first —
	// creating pb_data/ and applying migrations in whatever directory the user
	// happens to be standing in — as a side effect of asking for a file to be
	// copied. PocketBase only skips bootstrap for --help and --version
	// (pocketbase.go, skipBootstrap) and offers no per-command opt-out, so the
	// short-circuit has to live here. The command is still added to the
	// command line below so that `pocketcqrs --help` lists it.
	if len(os.Args) > 1 && os.Args[1] == "skill" {
		sc := newSkillCommand()
		sc.SetArgs(os.Args[2:])
		if err := sc.Execute(); err != nil {
			os.Exit(1)
		}
		return
	}
	host.Run(host.Options{Commands: []*cobra.Command{newSkillCommand()}})
}
