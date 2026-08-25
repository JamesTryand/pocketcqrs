// Package localstore wires the two event.Store shapes every out-of-process
// component in this repo needs: a read-only poll source over the upstream
// pocketcqrs deployment's events.db, and a separate, locally-writable store
// for this component's own checkpoints and dead letters.
//
// The local store is opened with events.Open, the same constructor core
// itself uses — reusing that schema is deliberate (CLAUDE.md asks components
// here to "reuse pocketcqrs's existing dead-letter shape/semantics", and this
// is the simplest way: pocketcqrs deadletter list/retry/resolve works
// against the file for free). It is never used to poll or Append; it is
// opened purely for its Checkpoint/SaveCheckpoint/AddDeadLetter methods, and
// deliberately never pointed at the upstream events.db, so this process can
// never become a second writer to it.
package localstore

import (
	"fmt"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/events"
)

// Stores holds the two event.Store handles a component needs: Source polls
// the upstream deployment's events.db read-only; Local is this component's
// own, locally-writable store for checkpoints and dead letters.
type Stores struct {
	Source *events.Store
	Local  *events.Store
}

// Open opens both stores: sourcePath is the upstream pocketcqrs deployment's
// events.db (opened read-only), localPath is this component's own SQLite
// file (created if absent). Close both via Stores.Close when done.
func Open(sourcePath, localPath string) (*Stores, error) {
	source, err := events.OpenReadOnly(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("localstore: open upstream events.db read-only at %s: %w", sourcePath, err)
	}
	local, err := events.Open(localPath)
	if err != nil {
		source.Close()
		return nil, fmt.Errorf("localstore: open local store at %s: %w", localPath, err)
	}
	return &Stores{Source: source, Local: local}, nil
}

// Close closes both stores.
func (s *Stores) Close() error {
	sErr := s.Source.Close()
	lErr := s.Local.Close()
	if sErr != nil {
		return sErr
	}
	return lErr
}

// Engine builds a consumers.Engine polling Source and checkpointing to
// Local — the NewEngineWithCheckpoints shape every out-of-process component
// in this repo uses instead of NewEngine, since the two stores are never the
// same file here.
func (s *Stores) Engine(logger func(string, ...any)) *consumers.Engine {
	return consumers.NewEngineWithCheckpoints(s.Source, s.Local, logger)
}
