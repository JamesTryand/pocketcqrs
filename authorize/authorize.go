// Package authorize is this project's port of command authorization —
// dotnetcqrs's own CommandAuthorizationGenerator (a generated
// (aggregate, command)-keyed policy table + evaluator) plus
// CqrsGatewayEndpoints.MapCqrsGateway's pluggable `authorize` hook — proved
// out first there, then ported.
//
// The shape transfers directly, as predicted: gateway.RegisterRoutes has
// exactly one generic dispatch path (POST /api/cqrs/{aggregate}/{id}/{command}),
// same as dotnetcqrs's MapCqrsGateway, so there is nowhere per-command to
// attach a check even if pocketcqrs generated per-command routes at all
// (it does not — see scaffold's own package doc: JS deciders are
// hand-authored/imported, not codegen'd per model). What differs from
// dotnetcqrs is WHERE the policy table comes from: there is no separate
// "generate" step here at all. BuildPolicies reads an already-linted
// emschema.Document directly at deployment boot time — Go decoding plus a
// map build, not text generation — because this project's whole schema
// story is already "parse the document, don't emit source for it" (see
// emschema's own package doc). One document, one BuildPolicies call, one
// Authorizer wired into gateway.Config.Authorize.
//
// Deciders CANNOT do this work themselves: functions/decider.go's own
// newDeciderVM comment is explicit that a decider VM has "NO pb bindings" —
// deciders decide from command+state only, deliberately no I/O. Read-model
// lookups (the ownership/scope kinds below need one) only exist at the
// gateway layer, exactly the same architectural fact that put
// dotnetcqrs's own evaluator in a new CommandAuthorization.cs beside
// Program.cs rather than inside a generated decider.
package authorize

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/cmdauth"
	"github.com/jamestryand/pocketcqrs/emschema"
	"github.com/jamestryand/pocketcqrs/pii"
)

// The policy table's types are package cmdauth's, which evaluates them the
// same way here and in verify (emschema.Verify).
type (
	Key                  = cmdauth.Key
	Policy               = cmdauth.Policy
	FieldGatedRolePolicy = cmdauth.FieldGatedRolePolicy
	OwnershipPolicy      = cmdauth.OwnershipPolicy
	ScopePolicy          = cmdauth.ScopePolicy
)

// BuildPolicies is emschema.CommandPolicies: one document's policy table,
// keyed by the (aggregate, command) pair a gateway request names.
func BuildPolicies(doc *emschema.Document) (map[Key]Policy, error) {
	return emschema.CommandPolicies(doc)
}

// Authorizer evaluates a built policy table against a real request. Both
// resolver funcs are pluggable, not resolved from any fixed PocketBase
// field/claim — mirroring dotnetcqrs's own resolveOwnRole/resolveOwnSubjectId
// delegates and the same "auth is deliberately not built into this
// library" split this project's own authverify package already holds for
// authentication itself: which claim carries a role or the caller's own
// subject id is the host's to say, so the deployment supplies both at its
// own wiring site. Either may be
// nil, in which case the corresponding resolved value is "" — the same
// "no actor" posture an anonymous caller already produces if
// gateway.Config.AllowAnonymous permits the request through at all.
//
// ErasureRequest is the document's dataSubjects.erasure
// (BuildErasureRequest): who may request a subject's erasure. Nil lets no one.
type Authorizer struct {
	Policies            map[Key]Policy
	ErasureRequest      *cmdauth.ErasureRequestPolicy
	ResolveOwnRole      func(auth *core.Record) string
	ResolveOwnSubjectID func(auth *core.Record) string
}

// BuildErasureRequest is emschema.ErasureRequestPolicy: one document's
// dataSubjects.erasure, for Authorizer.ErasureRequest.
func BuildErasureRequest(doc *emschema.Document) (*cmdauth.ErasureRequestPolicy, error) {
	return emschema.ErasureRequestPolicy(doc)
}

// Authorize evaluates the declared policy for (aggregate, command), if any.
// No declared policy (the common case for most commands) returns
// (true, nil) — unchanged behavior for a document declaring none of these
// four keywords on this command. See cmdauth.Evaluate for how the
// declarations combine.
//
// The built-in dataSubject aggregate (package pii) is the exception: erasure
// can't be undone, so its commands are refused unless ErasureRequest admits
// one, and it only ever admits RequestErasure (schema 3.7.1). Erasing
// directly, holding, approving and legal holds are each host's own policy,
// never a document's: a host that allows them decides so in its own
// gateway.Config.Authorize before calling this.
//
// A refusal is (false, nil), or (false, *cmdauth.Refusal) when the refusing
// declaration names its own message (a scope's), which the gateway answers
// with in place of its own.
func (a *Authorizer) Authorize(app core.App, aggregate, command, id string, payload []byte, auth *core.Record) (bool, error) {
	ownRole, ownSubjectID := "", ""
	if a.ResolveOwnRole != nil {
		ownRole = a.ResolveOwnRole(auth)
	}
	if a.ResolveOwnSubjectID != nil {
		ownSubjectID = a.ResolveOwnSubjectID(auth)
	}
	if strings.EqualFold(aggregate, pii.SubjectAggregate) {
		if command != pii.RequestErasureCommand {
			return false, nil
		}
		ok, err := cmdauth.MayRequestErasure(a.ErasureRequest, ownRole, ownSubjectID, id, records(app))
		if err != nil {
			return false, fmt.Errorf("authorize: %s/%s: %w", aggregate, command, err)
		}
		return ok, nil
	}

	policy, ok := a.Policies[Key{Aggregate: aggregate, Command: command}]
	if !ok {
		return true, nil
	}
	refusal, err := cmdauth.Evaluate(policy, ownRole, ownSubjectID, id, payload, records(app))
	switch {
	case err != nil:
		return false, fmt.Errorf("authorize: %s/%s: %w", aggregate, command, err)
	case refusal == nil:
		return true, nil
	case refusal.Message != "":
		return false, refusal
	}
	return false, nil
}

// records is a cmdauth.Lookup over app's collections.
func records(app core.App) cmdauth.Lookup {
	return func(collection string, match ...cmdauth.Match) (cmdauth.Row, error) {
		sort.Slice(match, func(i, j int) bool { return match[i].Field < match[j].Field })
		clauses := make([]string, len(match))
		params := dbx.Params{}
		for i, m := range match {
			name := fmt.Sprintf("p%d", i)
			clauses[i] = m.Field + " = {:" + name + "}"
			params[name] = m.Value
		}
		rec, err := app.FindFirstRecordByFilter(collection, strings.Join(clauses, " && "), params)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return rec, nil
	}
}
