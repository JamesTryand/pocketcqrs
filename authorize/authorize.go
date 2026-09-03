// Package authorize is this project's port of platform/command-authorization
// — dotnetcqrs's own CommandAuthorizationGenerator (a generated
// (aggregate, command)-keyed policy table + evaluator) plus
// CqrsGatewayEndpoints.MapCqrsGateway's pluggable `authorize` hook — proved
// out first there, in project/timesheets, per that issue's own "prove out
// on dotnetcqrs first, then port" ordering (the same ordering Finding 3
// used).
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
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/emschema"
	"github.com/jamestryand/pocketcqrs/scaffold"
)

// Key identifies one command's policy — the (aggregate, command) pair a
// gateway request actually names, e.g. ("time_entry", "FlagTimeEntry"), not
// the document's own kebab-case ids.
type Key struct {
	Aggregate string
	Command   string
}

// Policy is one command's declared authorization requirement. At most one
// of the four fields is non-nil/non-empty per command, mirroring
// emschema.Command's own requiredRole/fieldGatedRole/requiredOwnership/
// scope one-for-one — a zero Policy (returned for any (aggregate, command)
// BuildPolicies never saw) means no requirement, same additive-optional
// posture the source schema documents throughout.
type Policy struct {
	RequiredRole   []string
	FieldGatedRole *FieldGatedRolePolicy
	Ownership      *OwnershipPolicy
	Scope          *ScopePolicy
}

// FieldGatedRolePolicy is command.fieldGatedRole's resolved shape — see
// emschema.CommandFieldGatedRole.
type FieldGatedRolePolicy struct {
	Field        string
	Value        any
	RequiredRole []string
}

// OwnershipPolicy is command.requiredOwnership's resolved shape.
// Collection is already resolved from Via.ReadModelID to the actual
// PocketBase collection name (emschema.ReadModelCollectionName) — the
// same resolution mapScopes/mapFilters already do for read-model scopes
// and filters, applied here to a command's own read-model reference.
type OwnershipPolicy struct {
	Collection  string
	KeyField    string
	OwnerField  string
	BypassRoles []string
}

// ScopePolicy is command.scope's resolved shape: two read-model hops,
// both already resolved to their real collection names.
type ScopePolicy struct {
	ResolveCollection  string
	ResolveKeyField    string
	ResolveSelectField string
	MemberOfCollection string
	MemberOfMatchField string
	BypassRoles        []string
}

// BuildPolicies reads every command in doc that declares
// requiredRole/fieldGatedRole/requiredOwnership/scope into a policy table
// keyed by (aggregate, command) — the SAME (aggregate, PascalCase command
// name) pair emschema's own importer (mapping.go's commandName/aggregateFor)
// derives for a decider imported/scaffolded from the SAME document, so a
// policy built here lines up with a real deployment's routes without this
// package needing its own separate naming convention.
//
// doc should already be lint-clean (emschema.Lint(doc).Err() == nil) —
// BuildPolicies does not re-check referential integrity itself (Lint
// already does, including for the four keywords this package reads; see
// lintCommandAuth), but it DOES require every command declaring one of the
// four keywords to carry a non-empty Aggregate tag, since a policy with no
// aggregate to key on is meaningless here (unlike an ordinary command,
// where aggregateFor can fall back to an operator-supplied override at
// import time — this package has no such override mechanism, and a command
// worth authorizing is a command an operator already knows the aggregate
// of).
func BuildPolicies(doc *emschema.Document) (map[Key]Policy, error) {
	out := make(map[Key]Policy)
	for id, cmd := range doc.Commands {
		if len(cmd.RequiredRole) == 0 && cmd.FieldGatedRole == nil &&
			cmd.RequiredOwnership == nil && cmd.Scope == nil {
			continue
		}
		if cmd.Aggregate == "" {
			return nil, fmt.Errorf("authorize: command %q declares an authorization requirement but has no "+
				"aggregate tag — BuildPolicies cannot key a policy on nothing (unlike an ordinary command "+
				"import, there is no --aggregate override for this)", id)
		}
		key := Key{
			Aggregate: emschema.LowerFirst(scaffold.SanitizeName(cmd.Aggregate)),
			Command:   emschema.TypeName(cmd.Name, id),
		}
		policy := Policy{RequiredRole: append([]string(nil), cmd.RequiredRole...)}

		if fg := cmd.FieldGatedRole; fg != nil {
			policy.FieldGatedRole = &FieldGatedRolePolicy{
				Field: fg.Field, Value: fg.Value,
				RequiredRole: append([]string(nil), fg.RequiredRole...),
			}
		}
		if own := cmd.RequiredOwnership; own != nil {
			collection, err := resolveCollection(doc, id, "requiredOwnership.via", own.Via.ReadModelID)
			if err != nil {
				return nil, err
			}
			policy.Ownership = &OwnershipPolicy{
				Collection: collection, KeyField: own.Via.KeyField, OwnerField: own.Via.OwnerField,
				BypassRoles: append([]string(nil), own.BypassRoles...),
			}
		}
		if sc := cmd.Scope; sc != nil {
			resolveCol, err := resolveCollection(doc, id, "scope.resolveVia", sc.ResolveVia.ReadModelID)
			if err != nil {
				return nil, err
			}
			memberCol, err := resolveCollection(doc, id, "scope.memberOfVia", sc.MemberOfVia.ReadModelID)
			if err != nil {
				return nil, err
			}
			policy.Scope = &ScopePolicy{
				ResolveCollection: resolveCol, ResolveKeyField: sc.ResolveVia.KeyField,
				ResolveSelectField: sc.ResolveVia.SelectField,
				MemberOfCollection: memberCol, MemberOfMatchField: sc.MemberOfVia.MatchField,
				BypassRoles: append([]string(nil), sc.BypassRoles...),
			}
		}
		out[key] = policy
	}
	return out, nil
}

func resolveCollection(doc *emschema.Document, cmdID, field, readModelID string) (string, error) {
	rm, ok := doc.ReadModels[readModelID]
	if !ok {
		return "", fmt.Errorf("authorize: command %q: %s references read model %q, which does not exist "+
			"(should have been caught by emschema.Lint already — was doc linted before BuildPolicies?)",
			cmdID, field, readModelID)
	}
	return scaffold.SanitizeName(emschema.ReadModelCollectionName(rm.Name, readModelID)), nil
}

// Authorizer evaluates a built policy table against a real request. Both
// resolver funcs are pluggable, not resolved from any fixed PocketBase
// field/claim — mirroring dotnetcqrs's own resolveOwnRole/resolveOwnStaffId
// delegates and the same "auth is deliberately not built into this
// library" split this project's own authverify package already holds for
// authentication itself: a role or "own staff id" is inherently
// project-specific (this package has no domain concept of "staff" at all),
// so the deployment supplies both at its own wiring site. Either may be
// nil, in which case the corresponding resolved value is "" — the same
// "no actor" posture an anonymous caller already produces if
// gateway.Config.AllowAnonymous permits the request through at all.
type Authorizer struct {
	Policies          map[Key]Policy
	ResolveOwnRole    func(auth *core.Record) string
	ResolveOwnStaffID func(auth *core.Record) string
}

// Authorize evaluates the declared policy for (aggregate, command), if any.
// No declared policy (the common case for most commands) returns
// (true, nil) — unchanged behavior for a document declaring none of these
// four keywords on this command. Implements the same bypass-then-resolve
// evaluation order dotnetcqrs's generated evaluator does, and in the same
// checked order (RequiredRole, then FieldGatedRole, then Ownership, then
// Scope) — a document only ever declares one of the four per command, but
// the order is fixed regardless so two authors reading this can predict it
// without checking which fields happen to be set.
func (a *Authorizer) Authorize(app core.App, aggregate, command, id string, payload []byte, auth *core.Record) (bool, error) {
	policy, ok := a.Policies[Key{Aggregate: aggregate, Command: command}]
	if !ok {
		return true, nil
	}

	ownRole := ""
	if a.ResolveOwnRole != nil {
		ownRole = a.ResolveOwnRole(auth)
	}

	if len(policy.RequiredRole) > 0 {
		return containsFold(policy.RequiredRole, ownRole), nil
	}

	if fg := policy.FieldGatedRole; fg != nil {
		var obj map[string]any
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &obj); err != nil {
				return false, fmt.Errorf("authorize: %s/%s: payload is not a JSON object: %w", aggregate, command, err)
			}
		}
		actual, present := obj[fg.Field]
		if !present || !valuesEqual(actual, fg.Value) {
			return true, nil // condition doesn't apply -- no additional requirement
		}
		return containsFold(fg.RequiredRole, ownRole), nil
	}

	ownStaffID := ""
	if a.ResolveOwnStaffID != nil {
		ownStaffID = a.ResolveOwnStaffID(auth)
	}

	if own := policy.Ownership; own != nil {
		if containsFold(own.BypassRoles, ownRole) {
			return true, nil
		}
		rec, err := app.FindFirstRecordByFilter(own.Collection, own.KeyField+" = {:id}", dbx.Params{"id": id})
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil // no such target row -- not authorized, same as dotnetcqrs's "owner is null"
		}
		if err != nil {
			return false, fmt.Errorf("authorize: %s/%s: resolving ownership: %w", aggregate, command, err)
		}
		owner := rec.GetString(own.OwnerField)
		return owner != "" && owner == ownStaffID, nil
	}

	if sc := policy.Scope; sc != nil {
		if containsFold(sc.BypassRoles, ownRole) {
			return true, nil
		}
		resolved, err := app.FindFirstRecordByFilter(sc.ResolveCollection, sc.ResolveKeyField+" = {:id}", dbx.Params{"id": id})
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("authorize: %s/%s: resolving scope: %w", aggregate, command, err)
		}
		selected := resolved.GetString(sc.ResolveSelectField)
		_, err = app.FindFirstRecordByFilter(sc.MemberOfCollection,
			sc.MemberOfMatchField+" = {:val} && staffId = {:staff}",
			dbx.Params{"val": selected, "staff": ownStaffID})
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("authorize: %s/%s: resolving scope membership: %w", aggregate, command, err)
		}
		return true, nil
	}

	return true, nil
}

func containsFold(roles []string, role string) bool {
	if role == "" {
		return false
	}
	for _, r := range roles {
		if strings.EqualFold(r, role) {
			return true
		}
	}
	return false
}

// valuesEqual compares a decoded JSON payload value against a
// CommandFieldGatedRole.Value the same way — string/bool/number — the
// generated evaluators everywhere else in this proposal do: a JSON-level
// literal match, not a type-coercing one. encoding/json decodes both sides'
// numbers as float64, so a plain == is correct for all three JSON value
// kinds the schema allows here.
func valuesEqual(actual, want any) bool {
	return actual == want
}
