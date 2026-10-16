package pii

import (
	"context"
	"fmt"
	"os"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/kms"
	"github.com/jamestryand/pocketcqrs/outbox"
)

// WireOptions is what a host hands Wire.
type WireOptions struct {
	Registry *decider.Registry
	Engine   *consumers.Engine
	// Checkpoints is the engine's durable checkpoint store (the evictor is
	// seeded from the key destroyer's position in it).
	Checkpoints consumers.CheckpointStore
	// Loader reads the local event store, for the erased-subject check.
	Loader decider.StreamLoader
	// Writer runs the key destroyer: true on the node that appends (the
	// master), false on a read-only secondary. The evictor runs everywhere.
	Writer bool
	// KMS, when nil, is an HTTPClient on KMS_FACADE_URL. There is no
	// in-memory fallback: a host that stores personal data and has no
	// facade must refuse to start, not quietly use a stand-in.
	KMS           kms.Client
	CacheCapacity int
	// Outbox, when set, takes the key service calls off the engine's pass
	// (R4): the key destroyer records each erasure there and Wire runs its
	// worker until ctx is done, so hand Wire a context that lives as long
	// as the host. Writer only; nil calls the key service from the pass.
	Outbox *outbox.Store
}

// Wire sets up everything a host storing personal data needs and returns
// the Deps every generated protector takes: the key-service client, the
// per-process reveal cache, the built-in data-subject aggregate, the key
// destroyer (writer only) and the cache evictor (every node).
//
// A dataSubject decider already registered is kept: a module that governs
// erasure (package retention) registers its own, which adds refusals to the
// core's, and the order a host wires them in must not drop them.
func Wire(ctx context.Context, o WireOptions) (Deps, error) {
	client := o.KMS
	if client == nil {
		url := os.Getenv(kms.EnvFacadeURL)
		if url == "" {
			return Deps{}, fmt.Errorf("pii: %s is not set; a host that stores personal data needs the key-management facade "+
				"and will not start without it", kms.EnvFacadeURL)
		}
		c, err := kms.NewHTTPClient(url, nil)
		if err != nil {
			return Deps{}, err
		}
		client = c
	}
	cache := NewCache(o.CacheCapacity)
	if !o.Registry.Has(SubjectAggregate) {
		RegisterDataSubjects(o.Registry)
	}
	if o.Writer {
		destroyer := KeyDestroyer{KMS: client, Outbox: o.Outbox}
		o.Engine.Register(destroyer)
		if o.Outbox != nil {
			worker := &outbox.Worker{Store: o.Outbox, Handlers: map[string]outbox.Handler{KeyDestroyerName: destroyer.Destroy}}
			go worker.Run(ctx)
		}
	}
	if _, err := RegisterCacheEvictor(ctx, o.Engine, cache, o.Checkpoints, o.Outbox); err != nil {
		return Deps{}, fmt.Errorf("pii: register cache evictor: %w", err)
	}
	return Deps{KMS: client, Cache: cache, Subjects: StoreSubjectStatus{Loader: o.Loader}}, nil
}
