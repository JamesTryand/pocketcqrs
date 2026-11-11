package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoWiringPackage(t *testing.T) {
	for in, want := range map[string]string{"domain": "domain", "Gen": "gen", "my-domain": "mydomain", "9x": "domain", "": "domain"} {
		if got := GoWiringPackage(in); got != want {
			t.Errorf("GoWiringPackage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGoWiringNamesEveryPart(t *testing.T) {
	plain := sampleDomain()
	plain.Reactors = []Reactor{{Name: "autoClose", On: []string{"TicketOpened"}, Aggregate: "audit", Command: "Record", IDPrefix: "audit-"}}
	src, err := GenerateGoWiring(GoWiring{Package: "domain", ImportBase: "example.com/app/domain", ModelFile: "model.eventmodel.json"},
		[]Domain{plain, piiDomain()})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"DO NOT EDIT",
		"h.DeclareCommands(doc)",
		"h.EnsureCollections(Collections()...)",
		"h.WirePII()",
		"decider.RegisterProtected(h.Registry, \"" + piiDomain().Aggregate + "\"",
		"decider.Register(h.Registry, \"" + plain.Aggregate + "\"",
		"h.AddReactor(",
		"searchindex.Register(",
		"queryroute.Register(se,",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("wiring lacks %q:\n%s", want, src)
		}
	}
	// the boundary (decision: sources are surface tooling's): no schedule, URL
	// or credential is ever generated
	for _, never := range []string{"cron", "http://", "https://", "Cron()"} {
		if strings.Contains(src, never) {
			t.Errorf("wiring names a source (%q):\n%s", never, src)
		}
	}
}

// TestGeneratedWiringCompiles builds a program of one's own outside this
// module: the aggregates' packages, the wiring above them and a main.go of two
// lines, against this checkout through a replace (offline).
func TestGeneratedWiringCompiles(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	plain := sampleDomain()
	plain.Reactors = []Reactor{{Name: "autoClose", On: []string{"TicketOpened"}, Aggregate: "audit", Command: "Record", IDPrefix: "audit-"}}
	writeOnly := Domain{Aggregate: "audit", Commands: []Command{{Name: "Record", Events: []Event{{Name: "Recorded", NoFields: true}}}}}

	for name, domains := range map[string][]Domain{
		"plain and personal data": {plain, piiDomain()},
		"no read models":          {writeOnly},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(rel, content string) {
				t.Helper()
				p := filepath.Join(dir, rel)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("go.mod", "module example.com/app\n\ngo 1.25.0\n\nrequire github.com/jamestryand/pocketcqrs v0.0.0\n\n"+
				"replace github.com/jamestryand/pocketcqrs => "+filepath.ToSlash(repoRoot)+"\n")
			write("main.go", "package main\n\nimport (\n\t\"github.com/jamestryand/pocketcqrs/host\"\n\n\t\"example.com/app/domain\"\n)\n\n"+
				"func main() {\n\td := domain.New()\n\thost.Run(host.Options{Register: d.Register, Serve: d.Serve})\n}\n")
			write("domain/model.eventmodel.json", "{}")
			src, err := GenerateGoWiring(GoWiring{Package: "domain", ImportBase: "example.com/app/domain", ModelFile: "model.eventmodel.json"}, domains)
			if err != nil {
				t.Fatal(err)
			}
			write("domain/"+GoWiringFile, src)
			for _, d := range domains {
				files, err := d.GenerateGo()
				if err != nil {
					t.Fatal(err)
				}
				for _, f := range files {
					write(filepath.Join("domain", GoPackageName(d.Aggregate), f.Name), f.Source)
				}
			}
			for _, args := range [][]string{{"mod", "tidy"}, {"build", "./..."}} {
				cmd := exec.Command("go", args...)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("go %s: %v\n%s\n--- register.go\n%s", strings.Join(args, " "), err, out, src)
				}
			}
		})
	}
}
