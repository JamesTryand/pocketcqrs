package emschema

import (
	"fmt"

	"github.com/jamestryand/pocketcqrs/cmdauth"
	"github.com/jamestryand/pocketcqrs/scaffold"
)

// CommandPolicies reads every command in doc that declares
// requiredRole/fieldGatedRole/requiredOwnership/scope into a policy table
// keyed by (aggregate, command) — the SAME (aggregate, PascalCase command
// name) pair the importer (mapping.go's commandName/aggregateFor) derives
// for a decider imported/scaffolded from the SAME document, so a policy
// built here lines up with a real deployment's routes without a separate
// naming convention.
//
// doc should already be lint-clean (Lint(doc).Err() == nil) — this does not
// re-check referential integrity itself (Lint already does, including for
// the four keywords read here; see lintCommandAuth), but it DOES require
// every command declaring one of the four keywords to carry a non-empty
// Aggregate tag, since a policy with no aggregate to key on is meaningless
// (unlike an ordinary command, where aggregateFor can fall back to an
// operator-supplied override at import time — there is no such override
// here, and a command worth authorizing is a command an operator already
// knows the aggregate of).
func CommandPolicies(doc *Document) (map[cmdauth.Key]cmdauth.Policy, error) {
	out := make(map[cmdauth.Key]cmdauth.Policy)
	for id, cmd := range doc.Commands {
		policy, declared, err := CommandPolicy(doc, id)
		if err != nil {
			return nil, err
		}
		if !declared {
			continue
		}
		if cmd.Aggregate == "" {
			return nil, fmt.Errorf("authorize: command %q declares an authorization requirement but has no "+
				"aggregate tag — BuildPolicies cannot key a policy on nothing (unlike an ordinary command "+
				"import, there is no --aggregate override for this)", id)
		}
		key := cmdauth.Key{
			Aggregate: LowerFirst(scaffold.SanitizeName(cmd.Aggregate)),
			Command:   TypeName(cmd.Name, id),
		}
		out[key] = policy
	}
	return out, nil
}

// CommandPolicy is one command's policy, and whether it declares any
// requirement at all.
func CommandPolicy(doc *Document, id string) (cmdauth.Policy, bool, error) {
	cmd, ok := doc.Commands[id]
	if !ok || len(cmd.RequiredRole) == 0 && cmd.FieldGatedRole == nil &&
		cmd.RequiredOwnership == nil && cmd.Scope == nil {
		return cmdauth.Policy{}, false, nil
	}
	policy := cmdauth.Policy{RequiredRole: append([]string(nil), cmd.RequiredRole...)}

	if fg := cmd.FieldGatedRole; fg != nil {
		policy.FieldGatedRole = &cmdauth.FieldGatedRolePolicy{
			Field: fg.Field, Value: fg.Value,
			RequiredRole: append([]string(nil), fg.RequiredRole...),
		}
	}
	// stateField ownership (schema 3.15.0) is decided by the aggregate
	// itself, from its own stream: no policy here reads a projection
	if own := cmd.RequiredOwnership; own != nil && own.Via != nil {
		collection, err := policyCollection(doc, id, "requiredOwnership.via", own.Via.ReadModelID)
		if err != nil {
			return cmdauth.Policy{}, false, err
		}
		policy.Ownership = &cmdauth.OwnershipPolicy{
			Collection: collection, KeyField: own.Via.KeyField, OwnerField: own.Via.OwnerField,
			BypassRoles: append([]string(nil), own.BypassRoles...),
		}
	}
	if sc := cmd.Scope; sc != nil {
		resolveCol, err := policyCollection(doc, id, "scope.resolveVia", sc.ResolveVia.ReadModelID)
		if err != nil {
			return cmdauth.Policy{}, false, err
		}
		memberCol, err := policyCollection(doc, id, "scope.memberOfVia", sc.MemberOfVia.ReadModelID)
		if err != nil {
			return cmdauth.Policy{}, false, err
		}
		policy.Scope = &cmdauth.ScopePolicy{
			ResolveCollection: resolveCol, ResolveKeyField: sc.ResolveVia.KeyField,
			ResolveSelectField: sc.ResolveVia.SelectField,
			MemberOfCollection: memberCol, MemberOfMatchField: sc.MemberOfVia.MatchField,
			MemberOfActorField: sc.MemberOfVia.ActorField,
			BypassRoles:        append([]string(nil), sc.BypassRoles...),
			Message:            sc.Message,
		}
	}
	return policy, true, nil
}

// ErasureRequestPolicy reads doc's dataSubjects.erasure: who may request a
// subject's erasure. Nil when the document declares none, and then no one
// may. doc should already be lint-clean (see lintDataSubjects).
func ErasureRequestPolicy(doc *Document) (*cmdauth.ErasureRequestPolicy, error) {
	e := doc.DataSubjects.erasureOrNil()
	if e == nil {
		return nil, nil
	}
	rule := &cmdauth.ErasureRequestPolicy{Self: e.Self, Roles: append([]string(nil), e.Roles...)}
	if v := e.Via; v != nil {
		rm, ok := doc.ReadModels[v.ReadModelID]
		if !ok {
			return nil, fmt.Errorf("authorize: dataSubjects.erasure.via references read model %q, which does not exist "+
				"(should have been caught by emschema.Lint already — was doc linted before BuildPolicies?)", v.ReadModelID)
		}
		rule.Via = &cmdauth.OwnershipPolicy{
			Collection: scaffold.SanitizeName(ReadModelCollectionName(rm.Name, v.ReadModelID)),
			KeyField:   v.KeyField, OwnerField: v.OwnerField,
		}
	}
	return rule, nil
}

func policyCollection(doc *Document, cmdID, field, readModelID string) (string, error) {
	rm, ok := doc.ReadModels[readModelID]
	if !ok {
		return "", fmt.Errorf("authorize: command %q: %s references read model %q, which does not exist "+
			"(should have been caught by emschema.Lint already — was doc linted before BuildPolicies?)",
			cmdID, field, readModelID)
	}
	return scaffold.SanitizeName(ReadModelCollectionName(rm.Name, readModelID)), nil
}
