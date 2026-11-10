package host

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"

	"github.com/jamestryand/pocketcqrs/cmdauth"
	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/emschema"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/outbox"
	"github.com/jamestryand/pocketcqrs/pii"
	"github.com/jamestryand/pocketcqrs/projections"
)

// Options is what an embedder adds to the stock program. The zero value is
// the stock program without its `skill` command.
//
// A host's read-model collections are ordinary PocketBase migrations: register
// them (github.com/pocketbase/pocketbase/migrations.Register, usually from an
// init function in the host's own package) before calling Run. They run with
// the platform's own, in file-name order, before Register is called.
type Options struct {
	// Commands are added to the command line, after the platform's own.
	Commands []*cobra.Command

	// Register runs once per process, for every command that bootstraps the
	// app (`serve`, and one-shot commands such as `superuser`), after the
	// event store, decider registry and consumer engine exist and before the
	// JS functions load: a JS decider that takes one of its aggregates' names
	// is refused like any other collision. Register deciders, projections and
	// command permissions here (see Host). An error stops the program.
	Register func(h *Host) error

	// Serve runs inside PocketBase's OnServe, after the platform's routes are
	// registered and before the node listens: the place for read routes
	// (queryroute.Register) and anything else served. An error stops the
	// program.
	Serve func(se *core.ServeEvent, h *Host) error
}

// Host is what the Options hooks are handed: what the bootstrap built, and
// the few things an embedder adds. Its fields are set before Register runs.
type Host struct {
	App      core.App
	Store    *events.Store
	Registry *decider.Registry
	Engine   *consumers.Engine
	// Context lives as long as the process; background work started with it
	// stops when the program shuts down.
	Context context.Context
	DataDir string
	// Writer is true on the node that appends (--cqrsRole=master) and false
	// on a read-only secondary, which must not run side effects.
	Writer bool
	// RoleField and SubjectField are --cqrsRoleField and --cqrsSubjectField:
	// the auth record fields a caller's role and own subject id are read from.
	RoleField, SubjectField string

	// AuthorizeSubjectCommand decides the built-in dataSubject aggregate's
	// commands other than RequestErasure (EraseSubject, and a module's such as
	// holds and approvals), after the superuser check. nil refuses them, as the
	// stock server does: who may erase whom is the host's own rule.
	AuthorizeSubjectCommand func(re *core.RequestEvent, command, id string, payload []byte) (bool, error)

	checkpoints    consumers.CheckpointStore
	projections    []projections.Projection
	policies       map[cmdauth.Key]cmdauth.Policy
	erasureRequest *cmdauth.ErasureRequestPolicy
	closers        []io.Closer
}

// AddProjection registers a compiled-in projection on the engine, and makes
// its collections write-guarded and the projection rebuildable, as the
// platform's own are.
func (h *Host) AddProjection(p projections.Projection) {
	h.Engine.Register(p)
	h.projections = append(h.projections, p)
}

// DeclareCommands enforces the command permissions an EventModel document
// declares (requiredRole, fieldGatedRole, requiredOwnership, scope) at the
// gateway, beside those the JS deciders declare, and admits RequestErasure as
// its dataSubjects.erasure says. Call it from Register, once per document.
// Unlike the JS deciders' table it is not replaced by a hot reload: compiled
// code does not reload. A command no document names stays allowed.
func (h *Host) DeclareCommands(doc *emschema.Document) error {
	policies, err := emschema.CommandPolicies(doc)
	if err != nil {
		return err
	}
	if h.policies == nil {
		h.policies = map[cmdauth.Key]cmdauth.Policy{}
	}
	for k, p := range policies {
		if _, dup := h.policies[k]; dup {
			return fmt.Errorf("command %s.%s is declared twice", k.Aggregate, k.Command)
		}
		h.policies[k] = p
	}
	rule, err := emschema.ErasureRequestPolicy(doc)
	if err != nil {
		return err
	}
	if rule != nil {
		if h.erasureRequest != nil {
			return fmt.Errorf("dataSubjects.erasure is declared by more than one document")
		}
		h.erasureRequest = rule
	}
	return nil
}

// OwnRole is the caller's role, read as the gateway reads it ("" for none):
// the same resolver a read route's queryroute.Config.ResolveOwnRole needs.
func (h *Host) OwnRole(auth *core.Record) string {
	if auth == nil {
		return ""
	}
	field := h.RoleField
	if field == "" {
		field = "role"
	}
	return auth.GetString(field)
}

// OwnSubjectID is the caller's own data-subject id ("" for none).
func (h *Host) OwnSubjectID(auth *core.Record) string {
	if auth == nil {
		return ""
	}
	if h.SubjectField != "" {
		return auth.GetString(h.SubjectField)
	}
	return auth.Id
}

// WirePII sets up personal data (pii.Wire) the way the bootstrap needs it:
// on the platform's registry, engine and checkpoints, with the key destroyer
// only on the writer, and key destruction taken off the engine's pass through
// an outbox of its own (pii-outbox.db in the data directory), whose worker
// stops with the program. It needs KMS_FACADE_URL; without it the program
// does not start. Call it once, from Register, before registering any
// protected aggregate.
func (h *Host) WirePII() (pii.Deps, error) {
	opts := pii.WireOptions{Registry: h.Registry, Engine: h.Engine, Checkpoints: h.checkpoints,
		Loader: h.Store, Writer: h.Writer}
	if h.Writer {
		box, err := outbox.Open(filepath.Join(h.DataDir, "pii-outbox.db"))
		if err != nil {
			return pii.Deps{}, err
		}
		h.closers = append(h.closers, box)
		opts.Outbox = box
	}
	return pii.Wire(h.Context, opts)
}

// OnClose closes c when the program shuts down, after the background loops
// have stopped (a search index's store, for example).
func (h *Host) OnClose(c io.Closer) { h.closers = append(h.closers, c) }

func (h *Host) close() {
	for i := len(h.closers) - 1; i >= 0; i-- {
		_ = h.closers[i].Close()
	}
}
