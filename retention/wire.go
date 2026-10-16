package retention

import (
	"context"
	"errors"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/outbox"
)

// WireOptions is what a host hands Wire.
type WireOptions struct {
	Registry *decider.Registry
	Engine   *consumers.Engine
	// Loader reads the local event store: the governor decides from the
	// subject's own stream.
	Loader decider.StreamLoader
	// Policy is the business's retention policy. Required: a host with
	// nothing to retain says so with AllowAll.
	Policy Policy
	// Outbox, when set, holds the reviews of held requests, and Wire runs
	// its worker until ctx is done, so hand Wire a context that lives as
	// long as the host. Nil reviews none.
	Outbox *outbox.Store
	Logger func(string, ...any)
}

// Wire governs erasure on the writer: it registers the governed dataSubject
// decider in place of the core one (pii.Wire keeps it, whichever is called
// first) and the governor, and runs the review worker when there is an
// outbox. The host still wires pii (its key destroyer is what erases), and
// its gateway Authorize hook must cover every command in Commands.
func Wire(ctx context.Context, o WireOptions) (*Governor, error) {
	if o.Policy == nil {
		return nil, errors.New("retention: WireOptions.Policy is required; a host with nothing to retain uses AllowAll")
	}
	RegisterDataSubjects(o.Registry)
	g := &Governor{Registry: o.Registry, Loader: o.Loader, Policy: o.Policy, Outbox: o.Outbox, Logger: o.Logger}
	o.Engine.Register(g)
	if o.Outbox != nil {
		w := &outbox.Worker{Store: o.Outbox, Handlers: map[string]outbox.Handler{ReviewConsumer: g.Review}, Logger: o.Logger}
		go w.Run(ctx)
	}
	return g, nil
}
