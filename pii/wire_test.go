package pii_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/kms"
	"github.com/jamestryand/pocketcqrs/pii"
)

func wireStore(t *testing.T) *events.Store {
	t.Helper()
	s, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestWireRefusesToStartWithoutAFacade(t *testing.T) {
	t.Setenv(kms.EnvFacadeURL, "")
	store := wireStore(t)
	_, err := pii.Wire(context.Background(), pii.WireOptions{
		Registry: decider.NewRegistry(store), Engine: consumers.NewEngine(store, nil),
		Checkpoints: store, Loader: store, Writer: true,
	})
	if err == nil || !strings.Contains(err.Error(), kms.EnvFacadeURL) {
		t.Fatalf("no facade, no start (never an in-memory fallback): %v", err)
	}
}

func TestWireRegistersSubjectsDestroyerAndEvictor(t *testing.T) {
	t.Setenv(kms.EnvFacadeURL, "https://kms.example.test/")
	for _, writer := range []bool{true, false} {
		store := wireStore(t)
		reg := decider.NewRegistry(store)
		engine := consumers.NewEngine(store, nil)
		deps, err := pii.Wire(context.Background(), pii.WireOptions{
			Registry: reg, Engine: engine, Checkpoints: store, Loader: store, Writer: writer,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := deps.KMS.(*kms.HTTPClient); !ok || deps.Cache == nil || deps.Subjects == nil {
			t.Fatalf("deps: %+v", deps)
		}
		if !reg.Has(pii.SubjectAggregate) {
			t.Fatal("the data-subject aggregate must be registered")
		}
		names := strings.Join(engine.Names(), ",")
		if strings.Contains(names, pii.KeyDestroyerName) != writer {
			t.Fatalf("writer=%v: key destroyer only on the writer, got %s", writer, names)
		}
		if !strings.Contains(names, "pii:cache-evictor") {
			t.Fatalf("the evictor runs on every node: %s", names)
		}
	}
}
