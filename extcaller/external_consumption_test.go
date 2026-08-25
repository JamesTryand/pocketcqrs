package extcaller

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestExternalModuleCanConsumeExtcaller is the regression test for the
// "extcaller cannot be consumed as an external Go module dependency" fix
// (see NEEDS.md): it builds a throwaway module, completely outside this
// repo's module graph, that depends on this package via a replace directive
// and supplies its own Gateway implementation -- without ever importing
// internal/gatewayclient, which Go's own internal-package rule makes
// unreachable from an external module regardless of any replace directive.
// Before the fix, Config.Gateway required *gatewayclient.Client directly,
// so this would fail to compile.
func TestExternalModuleCanConsumeExtcaller(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	goMod := "module extconsumer-smoke\n\ngo 1.25.0\n\n" +
		"require github.com/jamestryand/pocketcqrs v0.0.0\n\n" +
		"replace github.com/jamestryand/pocketcqrs => " + repoRoot + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}

	main := `package main

import (
	"context"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/outbound"

	"github.com/jamestryand/pocketcqrs/extcaller"
)

type myGateway struct{}

func (myGateway) Dispatch(ctx context.Context, cmd extcaller.Command) error { return nil }

func main() {
	outClient, err := outbound.New(outbound.Config{
		AllowedHosts: []string{"example.com"},
		MaxInFlight:  1,
		MaxBodyBytes: 1 << 10,
	})
	if err != nil {
		panic(err)
	}
	store, err := events.Open("smoke.db")
	if err != nil {
		panic(err)
	}
	defer store.Close()

	if _, err := extcaller.New(extcaller.Config{
		Name:        "smoke",
		Outbound:    outClient,
		Gateway:     myGateway{},
		DeadLetters: store,
	}); err != nil {
		panic(err)
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy failed: %v\n%s", err, out)
	}

	build := exec.Command("go", "build", "./...")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("external module failed to build against extcaller: %v\n%s", err, out)
	}
}
