package consumers

import (
	"context"
	"sort"
	"testing"
)

// memRequests is a RebuildRequests in memory: a set, so a repeated request
// is one request.
type memRequests struct{ names map[string]bool }

func (m *memRequests) Pending(context.Context) ([]string, error) {
	var out []string
	for n := range m.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}
func (m *memRequests) Done(_ context.Context, name string) error {
	delete(m.names, name)
	return nil
}

// owning is a recorder that owns collections, i.e. a projection.
type owning struct{ tiered }

func (o *owning) Collections() []string { return []string{o.name + "_rows"} }

// A rebuild is a request the engine consumes between passes: it wipes the
// projection (through the host's Wipe), resets its checkpoint to 0, takes
// the request off, and the same pass replays it from the start. The engine
// stays the only thing that runs the projection, so nothing applies it
// twice. A repeated request is one request; a request for something that
// isn't a projection is dropped without wiping anything; one for a name
// this engine doesn't run stays for when it does.
func TestRebuildIsARequestTheEngineConsumes(t *testing.T) {
	var dir string
	store := openStore(t, &dir)
	defer store.Close()
	ctx := context.Background()
	appendOne(t, store, "t1")
	appendOne(t, store, "t2")

	proj := &owning{tiered{name: "tasks"}}
	reactor := &tiered{name: "notify", effect: true}
	requests := &memRequests{names: map[string]bool{}}
	var wiped []string
	engine := NewEngine(store, func(string, ...any) {})
	engine.Rebuilds = requests
	engine.Wipe = func(_ context.Context, c Consumer) error {
		wiped = append(wiped, c.Name())
		return nil
	}
	engine.Register(proj)
	engine.Register(reactor)
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(proj.applied) != 2 || len(wiped) != 0 {
		t.Fatalf("first pass: applied %v, wiped %v", proj.applied, wiped)
	}

	// requested twice, from two places: one rebuild
	requests.names["tasks"] = true
	requests.names["tasks"] = true
	requests.names["notify"] = true      // not a projection
	requests.names["searchIndex"] = true // not run by this engine (yet)
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(wiped) != 1 || wiped[0] != "tasks" {
		t.Fatalf("want exactly one wipe, of the projection: %v", wiped)
	}
	if len(proj.applied) != 4 {
		t.Fatalf("the projection replays both events again in the same pass: %v", proj.applied)
	}
	if len(reactor.applied) != 2 {
		t.Fatalf("a reactor is never replayed by a rebuild: %v", reactor.applied)
	}
	if pos, _ := store.Checkpoint(ctx, "tasks"); pos != 2 {
		t.Fatalf("the projection is caught up again: checkpoint %d", pos)
	}
	pending, _ := requests.Pending(ctx)
	if len(pending) != 1 || pending[0] != "searchIndex" {
		t.Fatalf("the projection's and the reactor's requests are taken off, the unknown one stays: %v", pending)
	}

	// nothing pending for it any more: the next pass doesn't rebuild again
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(wiped) != 1 || len(proj.applied) != 4 {
		t.Fatalf("a taken request is not repeated: wiped %v, applied %v", wiped, proj.applied)
	}
}

// If the wipe fails, nothing else happens: the checkpoint is not reset and
// the request stays, so the next pass tries again from the start.
func TestFailedWipeLeavesTheRequest(t *testing.T) {
	var dir string
	store := openStore(t, &dir)
	defer store.Close()
	ctx := context.Background()
	appendOne(t, store, "t1")

	proj := &owning{tiered{name: "tasks"}}
	requests := &memRequests{names: map[string]bool{}}
	engine := NewEngine(store, func(string, ...any) {})
	engine.Rebuilds = requests
	engine.Wipe = func(context.Context, Consumer) error { return context.DeadlineExceeded }
	engine.Register(proj)
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	requests.names["tasks"] = true
	_ = engine.RunOnce(ctx)
	if pos, _ := store.Checkpoint(ctx, "tasks"); pos != 1 {
		t.Fatalf("a failed wipe must not reset the checkpoint: %d", pos)
	}
	if len(proj.applied) != 1 {
		t.Fatalf("nothing replays after a failed wipe: %v", proj.applied)
	}
	if pending, _ := requests.Pending(ctx); len(pending) != 1 {
		t.Fatalf("the request stays for the next pass: %v", pending)
	}
}
