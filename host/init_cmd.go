package host

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/mod/module"
)

// pocketcqrsModulePath is this module's own path, which a program of your
// own requires and runs as a tool.
const pocketcqrsModulePath = "github.com/jamestryand/pocketcqrs"

// newInitCommand builds `init`: a starting point for a program of your own,
// in its own repository, that depends on pocketcqrs as a module version and
// never on its source. Like `schema import` it writes files and touches no
// running platform, so it is short-circuited before PocketBase bootstraps
// (see Run).
func newInitCommand() *cobra.Command {
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Start a program of your own (its own repository) on pocketcqrs",
	}
	goCmd := &cobra.Command{
		Use:   "go <module> --model <document.json>",
		Short: "Start a Go program with a document's domains compiled in, depending on pocketcqrs as a module",
		Long: "Writes a new Go module: go.mod (pocketcqrs as a requirement and as a tool, so one version\n" +
			"pins both the code generator and the library), main.go (host.Run with the generated\n" +
			"wiring), generate.go (the go:generate line), the document under model/, a Dockerfile for\n" +
			"a single static binary, and the generated code under domain/. Nothing is written into\n" +
			"pocketcqrs itself: your domain lives in your repository.\n\n" +
			"Then: go mod tidy && go build. To upgrade: go get -tool " + pocketcqrsModulePath + "@<version>\n" +
			"&& go generate ./... (docs/go-guide.md, \"Your own program\").",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			modPath := args[0]
			if err := module.CheckPath(modPath); err != nil {
				return fmt.Errorf("%q is not a module path: %w", modPath, err)
			}
			modelPath, _ := cmd.Flags().GetString("model")
			dir, _ := cmd.Flags().GetString("dir")
			version, _ := cmd.Flags().GetString("version")
			checkout, _ := cmd.Flags().GetString("pocketcqrs")
			if modelPath == "" {
				return fmt.Errorf("--model names the EventModel document whose domains the program runs")
			}
			if dir == "" {
				dir = path.Base(modPath)
			}
			if err := emptyOrAbsent(dir); err != nil {
				return err
			}
			req, err := initRequirement(version, checkout)
			if err != nil {
				return err
			}
			raw, err := os.ReadFile(modelPath)
			if err != nil {
				return fmt.Errorf("reading the document: %w", err)
			}
			modelName := filepath.Base(modelPath)
			if !strings.HasSuffix(modelName, ".json") {
				return fmt.Errorf("--model wants a single document (.json); a split manifest can be imported later with schema import")
			}
			name := path.Base(modPath)

			files := []struct{ name, content string }{
				{"go.mod", fmt.Sprintf("module %s\n\ngo 1.25.0\n\ntool %s\n\n%s", modPath, pocketcqrsModulePath, req)},
				{filepath.Join("model", modelName), string(raw)},
				{"generate.go", fmt.Sprintf(initGenerate, modelName)},
				{"main.go", fmt.Sprintf(initMain, name, modelName, modPath)},
				{"Dockerfile", fmt.Sprintf(initDockerfile, name, name, name)},
				{".gitignore", fmt.Sprintf("/pb_data/\n/%s\n/%s.exe\n", name, name)},
				{"README.md", fmt.Sprintf(initReadme, name, modelName, pocketcqrsModulePath)},
			}
			for _, f := range files {
				p := filepath.Join(dir, f.name)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(p, []byte(f.content), 0o644); err != nil {
					return err
				}
			}

			// the first generation, in-process: the same command go generate
			// runs, so the new directory builds as soon as its modules resolve
			imp := newSchemaImportCommand()
			imp.SilenceUsage = true
			imp.SetOut(cmd.OutOrStdout())
			imp.SetErr(cmd.ErrOrStderr())
			imp.SetArgs([]string{filepath.Join(dir, "model", modelName), "--lang", "go",
				"--out", filepath.Join(dir, "domain"), "--force"})
			if err := imp.Execute(); err != nil {
				// the directory was empty or absent: leave it so
				entries, _ := os.ReadDir(dir)
				for _, e := range entries {
					_ = os.RemoveAll(filepath.Join(dir, e.Name()))
				}
				return fmt.Errorf("the document could not be generated, so nothing was kept in %s: %w", dir, err)
			}
			cmd.Printf("\nStarted %s in %s. Next:\n  cd %s\n  go mod tidy\n  go build\n", modPath, dir, dir)
			if checkout != "" {
				cmd.Printf("\ngo.mod builds against the checkout %s (a replace line): fine for trying it, but remove the "+
					"replace and require a released version before you commit, or the repository depends on "+
					"pocketcqrs's source.\n", checkout)
			}
			return nil
		},
	}
	goCmd.Flags().String("model", "", "the EventModel document (.json) whose domains the program runs (copied into model/)")
	goCmd.Flags().String("dir", "", "the directory to write (must be empty or absent); default: the module path's last element")
	goCmd.Flags().String("version", "", "the pocketcqrs version to require; default: the version this binary was built from")
	goCmd.Flags().String("pocketcqrs", "", "build against this pocketcqrs checkout (a replace line), for trying an unreleased version")
	initCmd.AddCommand(goCmd)
	return initCmd
}

// initRequirement is the new module's requirement on pocketcqrs: a named
// version, else the version this binary was built from (as a module's tool or
// installed at a version), with a replace onto a checkout when one is named.
func initRequirement(version, checkout string) (string, error) {
	if version == "" {
		version = builtVersion()
	}
	if checkout != "" {
		abs, err := filepath.Abs(checkout)
		if err != nil {
			return "", err
		}
		if version == "" {
			version = "v0.0.0"
		}
		return fmt.Sprintf("require %s %s\n\nreplace %s => %s\n", pocketcqrsModulePath, version,
			pocketcqrsModulePath, filepath.ToSlash(abs)), nil
	}
	if version == "" {
		return "", fmt.Errorf("this pocketcqrs was not built from a released version, so name one (--version vX.Y.Z) " +
			"or a checkout to try it against (--pocketcqrs <dir>)")
	}
	return fmt.Sprintf("require %s %s\n", pocketcqrsModulePath, version), nil
}

// builtVersion is the pocketcqrs version this binary was built from, or "".
func builtVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	released := func(v string) bool { return v != "" && v != "(devel)" }
	if info.Main.Path == pocketcqrsModulePath && released(info.Main.Version) {
		return info.Main.Version
	}
	for _, d := range info.Deps {
		if d.Path == pocketcqrsModulePath && d.Replace == nil && released(d.Version) {
			return d.Version
		}
	}
	return ""
}

func emptyOrAbsent(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s is not empty: init writes a new program, never into an existing one", dir)
	}
	return nil
}

const initGenerate = `package main

// The generated code under domain/ comes from the document in model/, by the
// pocketcqrs version go.mod names (a tool dependency, so the generator and the
// library always match). Commit what it writes. To regenerate after changing
// the document, or after upgrading pocketcqrs:
//
//	go generate ./...

//go:generate go tool pocketcqrs schema import model/%s --lang go --out domain --force
`

const initMain = `// Command %s runs the domains of model/%s on pocketcqrs: the whole
// pocketcqrs program (serve, superuser, schema, ...) with them compiled in.
//
// What is yours goes here, around the generated wiring: extra commands
// (host.Options.Commands), extra registrations after d.Register (for example
// h.AuthorizeSubjectCommand, who may erase whom), extra routes after d.Serve.
package main

import (
	"github.com/jamestryand/pocketcqrs/host"

	"%s/domain"
)

func main() {
	d := domain.New()
	host.Run(host.Options{Register: d.Register, Serve: d.Serve})
}
`

const initDockerfile = `# A single static binary: the whole pocketcqrs program with this repository's
# domains compiled in. Its data lives in /data (mount a volume there).
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/%s .

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/%s /app/%s
EXPOSE 8090
VOLUME /data
ENTRYPOINT ["/app/` + "%[1]s" + `", "serve", "--http", "0.0.0.0:8090", "--dir", "/data/pb_data"]
`

const initReadme = "# %s\n\n" +
	"The domains of `model/%s`, compiled into one program on pocketcqrs.\n\n" +
	"pocketcqrs is a module dependency (`go.mod`), never a copy of its source: it is both required\n" +
	"(the library) and a tool (the code generator), so one version pins both.\n\n" +
	"- `model/`: the EventModel document. Change it, then `go generate ./...`.\n" +
	"- `domain/`: generated from it. Commit it; do not edit it. A rule the document leaves to you is\n" +
	"  written in its own file, from the `.go.example` beside the generated one.\n" +
	"- `main.go`: the program, and the place for what is your own.\n\n" +
	"Build: `go build`. Run: `./<binary> serve`. Upgrade pocketcqrs:\n\n" +
	"    go get -tool %s@<version>\n    go generate ./...\n"
