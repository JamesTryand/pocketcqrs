package emschema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
	"github.com/jamestryand/pocketcqrs/scaffold"
	"github.com/jamestryand/pocketcqrs/verifyharness/wire"
)

// pocketcqrsModule is this module's path, which the harness module requires.
const pocketcqrsModule = "github.com/jamestryand/pocketcqrs"

// goRun answers the verifier's decides and projection runs from Go code
// (VerifyOptions.Lang "go"). Verify runs the scenarios twice: the first pass
// only records what each would ask (collecting), one harness run answers it
// all, and the second pass compares, through the same code as the JS path.
type goRun struct {
	collecting bool
	in         wire.Input
	views      map[string]*wire.View
	// packages are the aggregates whose Go package is present, and
	// projections the collections a Go projection builds.
	packages    map[string]bool
	projections map[string]bool
	commands    map[string]wire.CommandResult
	answers     map[string]wire.ViewResult
	// failed is the harness not running at all (a build error, say): every
	// scenario reports it.
	failed string
}

func (g *goRun) decide(id, aggregate, stream string, given []events.Event, cmd decider.Command, meta map[string]any) (*functions.DecideDryRun, string, bool) {
	if !g.packages[aggregate] {
		return nil, "", true
	}
	if g.collecting {
		g.in.Commands = append(g.in.Commands, wire.Command{ID: id, Aggregate: aggregate, Stream: stream, Given: given, Command: cmd, Meta: meta})
		return &functions.DecideDryRun{}, "", false
	}
	if g.failed != "" {
		return nil, g.failed, false
	}
	out, ok := g.commands[id]
	switch {
	case !ok:
		return nil, "the Go harness gave no answer", false
	case out.Error != "":
		return nil, "the Go decider could not be asked: " + out.Error, false
	}
	run := &functions.DecideDryRun{Rejected: out.Rejected, Message: out.Message}
	for _, p := range out.Produced {
		run.Produced = append(run.Produced, events.NewEvent{Type: p.Type, Data: p.Data})
	}
	return run, "", false
}

func (g *goRun) project(id, collection string, fixture []events.Event) (map[string]map[string]any, string, bool) {
	if !g.projections[collection] {
		return nil, "", true
	}
	if g.collecting {
		v := g.views[id]
		if v == nil {
			v = &wire.View{ID: id}
			g.views[id] = v
		}
		for _, r := range v.Runs {
			if r.Collection == collection {
				return map[string]map[string]any{}, "", false
			}
		}
		v.Runs = append(v.Runs, wire.Run{Collection: collection, Events: fixture})
		return map[string]map[string]any{}, "", false
	}
	if g.failed != "" {
		return nil, g.failed, false
	}
	out, ok := g.answers[id]
	switch {
	case !ok:
		return nil, "the Go harness gave no answer", false
	case out.Error != "":
		return nil, "the Go projection run failed: " + out.Error, false
	}
	return out.Rows[collection], "", false
}

// goModule writes the harness module under dir: the code under test, a
// package per aggregate, and the generated test file naming its
// constructors. It returns which aggregates and collections it carries.
func goModule(dir string, mapped *Mapped, opts VerifyOptions) (packages, projections map[string]bool, err error) {
	require, err := pocketcqrsRequirement(opts.Pocketcqrs)
	if err != nil {
		return nil, nil, err
	}
	mod := "module pcqharness\n\ngo 1.25.0\n\n" + require
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		return nil, nil, err
	}
	packages, projections = map[string]bool{}, map[string]bool{}
	var imports, registers, builds []string
	for _, d := range mapped.Domains {
		pkg := scaffold.GoPackageName(d.Aggregate)
		dst := filepath.Join(dir, "gen", pkg)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return nil, nil, err
		}
		if opts.Host != "" {
			if ok, err := copyHostPackage(filepath.Join(opts.Host, pkg), dst); err != nil || !ok {
				if err != nil {
					return nil, nil, err
				}
				continue // the host has no package: its scenarios skip
			}
		} else {
			files, err := d.GenerateGo()
			if err != nil {
				return nil, nil, fmt.Errorf("generating %s: %w", d.Aggregate, err)
			}
			for _, f := range files {
				if strings.HasSuffix(f.Name, ".example") {
					continue
				}
				if err := os.WriteFile(filepath.Join(dst, f.Name), []byte(f.Source), 0o644); err != nil {
					return nil, nil, err
				}
			}
			if len(d.Hooks()) > 0 {
				if err := os.WriteFile(filepath.Join(dst, d.Aggregate+"_hand.go"), []byte(d.GoHandStandIn(pkg)), 0o644); err != nil {
					return nil, nil, err
				}
			}
		}
		packages[d.Aggregate] = true
		imports = append(imports, fmt.Sprintf("\t%s %q", pkg, "pcqharness/gen/"+pkg))
		ctor := scaffold.ExportName(d.Aggregate)
		if len(d.PIIFields()) > 0 {
			registers = append(registers, fmt.Sprintf("\t\tdecider.RegisterProtected(r, %q, %s.%s(), %s.NewProtector(deps))", d.Aggregate, pkg, ctor, pkg))
		} else {
			registers = append(registers, fmt.Sprintf("\t\tdecider.Register(r, %q, %s.%s())", d.Aggregate, pkg, ctor))
		}
		for _, rm := range d.ReadModels {
			if rm.Hand && opts.Host == "" {
				continue // not generated; its scenarios are HAND
			}
			projections[rm.Collection] = true
			builds = append(builds, fmt.Sprintf("\t\t%q: func(app core.App) verifyharness.Projection { return %s.New%s(app) },",
				rm.Collection, pkg, scaffold.ExportName(rm.Collection)))
		}
	}
	sort.Strings(imports)
	src := "package harness\n\nimport (\n\t\"testing\"\n\n\t\"github.com/pocketbase/pocketbase/core\"\n\n" +
		"\t\"github.com/jamestryand/pocketcqrs/decider\"\n\t\"github.com/jamestryand/pocketcqrs/pii\"\n" +
		"\t\"github.com/jamestryand/pocketcqrs/verifyharness\"\n\n" + strings.Join(imports, "\n") + "\n)\n\n" +
		"var _ = pii.Deps{}\n\nfunc TestHarness(t *testing.T) {\n" +
		"\tspec := verifyharness.Spec{\n\t\tRegister: func(r *decider.Registry, deps pii.Deps) {\n" +
		strings.Join(registers, "\n") + "\n\t\t},\n\t\tProjections: map[string]func(app core.App) verifyharness.Projection{\n" +
		strings.Join(builds, "\n") + "\n\t\t},\n\t}\n" +
		"\tif err := verifyharness.Main(spec); err != nil {\n\t\tt.Fatal(err)\n\t}\n}\n"
	if err := os.MkdirAll(filepath.Join(dir, "harness"), 0o755); err != nil {
		return nil, nil, err
	}
	return packages, projections, os.WriteFile(filepath.Join(dir, "harness", "harness_test.go"), []byte(src), 0o644)
}

// pocketcqrsRequirement is the harness module's requirement on pocketcqrs:
// a checkout (replace) when one is named, else the version this binary was
// built from.
func pocketcqrsRequirement(checkout string) (string, error) {
	if checkout != "" {
		abs, err := filepath.Abs(checkout)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("require %s v0.0.0\n\nreplace %s => %s\n", pocketcqrsModule, pocketcqrsModule, filepath.ToSlash(abs)), nil
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Path == pocketcqrsModule &&
		info.Main.Version != "" && info.Main.Version != "(devel)" {
		return fmt.Sprintf("require %s %s\n", pocketcqrsModule, info.Main.Version), nil
	}
	return "", fmt.Errorf("checking the Go code needs pocketcqrs's source to build against: this binary " +
		"wasn't built from a released version, so name a checkout (--pocketcqrs)")
}

// copyHostPackage copies a host package's Go files (not its tests); false
// when the host has no such package.
func copyHostPackage(src, dst string) (bool, error) {
	entries, err := os.ReadDir(src)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	any := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			return false, err
		}
		if err := os.WriteFile(filepath.Join(dst, name), raw, 0o644); err != nil {
			return false, err
		}
		any = true
	}
	return any, nil
}

// collections is every read model's collection, as the harness creates it.
func goCollections(mapped *Mapped) []wire.Collection {
	var out []wire.Collection
	for _, d := range mapped.Domains {
		for _, rm := range d.ReadModels {
			c := wire.Collection{Name: rm.Collection, Key: rm.Key, Fields: []wire.Field{{Name: rm.Key, Type: "text"}}}
			for _, f := range rm.Fields {
				if f.Name != rm.Key {
					c.Fields = append(c.Fields, wire.Field{Name: f.Name, Type: scaffold.StorageType(f), PII: f.PII})
				}
			}
			out = append(out, c)
			if rm.RowsPerListItem != nil {
				out = append(out, wire.Collection{Name: scaffold.ItemStreams(rm), Key: "stream",
					Fields: []wire.Field{{Name: "stream", Type: "text"}, {Name: "state", Type: "json"}}})
			}
		}
	}
	return out
}

// answer runs the harness module under dir over what the first pass
// recorded.
func (g *goRun) answer(dir string) {
	in := filepath.Join(dir, "in.json")
	out := filepath.Join(dir, "out.json")
	for _, v := range g.views {
		g.in.Views = append(g.in.Views, *v)
	}
	raw, err := json.Marshal(g.in)
	if err == nil {
		err = os.WriteFile(in, raw, 0o644)
	}
	if err != nil {
		g.failed = "the Go harness input could not be written: " + err.Error()
		return
	}
	env := append(os.Environ(), "GOFLAGS=-mod=mod", wire.EnvIn+"="+in, wire.EnvOut+"="+out)
	var log bytes.Buffer
	for _, args := range [][]string{{"mod", "tidy"}, {"test", "-count=1", "-timeout", "30m", "-run", "TestHarness", "./harness"}} {
		cmd := exec.Command("go", args...)
		cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, &log, &log
		if err := cmd.Run(); err != nil {
			g.failed = fmt.Sprintf("the Go code did not build or run (go %s): %s", args[0], tail(log.String(), 40))
			return
		}
	}
	raw, err = os.ReadFile(out)
	var answers wire.Output
	if err == nil {
		err = json.Unmarshal(raw, &answers)
	}
	if err != nil {
		g.failed = "the Go harness gave no answers: " + err.Error()
		return
	}
	g.commands, g.answers = map[string]wire.CommandResult{}, map[string]wire.ViewResult{}
	for _, c := range answers.Commands {
		g.commands[c.ID] = c
	}
	for _, v := range answers.Views {
		g.answers[v.ID] = v
	}
}

func tail(s string, lines int) string {
	all := strings.Split(strings.TrimSpace(s), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}
