package adminapi

import (
	"context"
	"errors"
	"testing"

	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/projections"
)

type namedProjection struct{ name string }

func (p namedProjection) Name() string                            { return p.name }
func (namedProjection) Collections() []string                     { return []string{"rows"} }
func (namedProjection) Apply(context.Context, events.Event) error { return nil }

// The CLI's `projection rebuild` and POST /api/cqrs/admin/projections/{name}
// /rebuild share RequestRebuild: it records a request for the engine, and
// nothing else. Only a projection this node runs can be requested; asking
// again is the same request.
func TestRequestRebuildOnlyRecords(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	requests, err := projections.NewRebuildRequests(app.DB())
	if err != nil {
		t.Fatal(err)
	}
	s := &State{App: app, Rebuilds: requests, GoProjections: []projections.Projection{namedProjection{"tasks"}}}
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := s.RequestRebuild(ctx, "tasks"); err != nil {
			t.Fatal(err)
		}
	}
	for _, unknown := range []string{"reactor:fulfillment", "nope"} {
		if err := s.RequestRebuild(ctx, unknown); !errors.Is(err, ErrUnknownProjection) {
			t.Errorf("%s: want ErrUnknownProjection, got %v", unknown, err)
		}
	}
	pending, err := requests.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0] != "tasks" {
		t.Fatalf("want one request, for tasks: %v", pending)
	}
}
