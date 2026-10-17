// Package cmdauth evaluates a command's declared authorization: the policy
// a document's requiredRole/fieldGatedRole/requiredOwnership/scope
// declarations resolve to (emschema.CommandPolicies), checked against a
// caller.
//
// It reads no store of its own. The two declarations that consult a read
// model (ownership via, scope) ask a Lookup, so the same evaluation runs at
// the gateway against PocketBase collections (package authorize) and in
// verify against rows projected from a scenario's given events
// (emschema.Verify). One evaluator, so a scenario checks what a deployment
// does.
package cmdauth

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Key identifies one command's policy — the (aggregate, command) pair a
// gateway request actually names, e.g. ("support_ticket", "EscalateTicket"), not
// the document's own kebab-case ids.
type Key struct {
	Aggregate string
	Command   string
}

// Policy is one command's declared authorization requirement, mirroring
// emschema.Command's own requiredRole/fieldGatedRole/requiredOwnership/
// scope one-for-one. A zero Policy means no requirement, same
// additive-optional posture the source schema documents throughout.
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
	// MemberOfActorField is the field of MemberOfCollection holding a
	// member's actor id: the document names it (schema 4.0.0).
	MemberOfActorField string
	BypassRoles        []string
	// Message is the refusal's message when the actor is not a member
	// (schema 4.0.0's scope.message); empty: NotAuthorized's.
	Message string
}

// Row is one read-model row a Lookup found. *core.Record is one.
type Row interface {
	GetString(field string) string
}

// Match is one field a Lookup's row must hold the value of.
type Match struct {
	Field string
	Value string
}

// Lookup finds the first row of collection holding every match, or nil
// when there is none.
type Lookup func(collection string, match ...Match) (Row, error)

// Refusal is a declared requirement refusing the command. Message is the
// declaration's own, or empty when it declares none.
type Refusal struct {
	Message string
}

func (r *Refusal) Error() string {
	if r.Message == "" {
		return "not authorized"
	}
	return r.Message
}

// NotAuthorized is the refusal's message when its declaration names none.
func NotAuthorized(command, aggregate string) string {
	return "not authorized to issue " + command + " on " + aggregate
}

// Evaluate checks every declaration of p for a caller holding role, whose
// own subject id is subjectID, issuing a command with payload to the
// aggregate id. It returns the first refusal, or nil when the command is
// authorized.
//
// The declarations combine: EVERY one a command declares must hold (the
// schema's docs say so, and dotnetcqrs's generated evaluator does the
// same since 6649745). They are checked in a fixed order (RequiredRole,
// FieldGatedRole, Ownership, Scope) and the first that fails refuses. A
// bypass role skips only its own declaration's check, and a field gate
// whose condition doesn't apply adds no requirement but ends nothing.
func Evaluate(p Policy, role, subjectID, id string, payload []byte, find Lookup) (*Refusal, error) {
	if r, err := CheckRoles(p, role, payload); r != nil || err != nil {
		return r, err
	}
	return CheckState(p, role, subjectID, id, find)
}

// CheckRoles is Evaluate's role declarations alone (RequiredRole,
// FieldGatedRole): what the caller's role and the payload decide.
func CheckRoles(p Policy, role string, payload []byte) (*Refusal, error) {
	if len(p.RequiredRole) > 0 && !containsFold(p.RequiredRole, role) {
		return &Refusal{}, nil
	}
	if fg := p.FieldGatedRole; fg != nil {
		var obj map[string]any
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &obj); err != nil {
				return nil, fmt.Errorf("payload is not a JSON object: %w", err)
			}
		}
		// a payload lacking the field, or carrying another value, means the
		// gate adds no requirement
		if actual, present := obj[fg.Field]; present && valuesEqual(actual, fg.Value) &&
			!containsFold(fg.RequiredRole, role) {
			return &Refusal{}, nil
		}
	}
	return nil, nil
}

// CheckState is Evaluate's read-model declarations alone (Ownership, Scope):
// what the target's rows say about who the caller is. role matters only as
// a bypass role.
func CheckState(p Policy, role, subjectID, id string, find Lookup) (*Refusal, error) {
	if own := p.Ownership; own != nil && !containsFold(own.BypassRoles, role) {
		row, err := find(own.Collection, Match{own.KeyField, id})
		if err != nil {
			return nil, fmt.Errorf("resolving ownership: %w", err)
		}
		// no such target row -- not authorized, same as dotnetcqrs's "owner is null"
		if row == nil {
			return &Refusal{}, nil
		}
		if owner := row.GetString(own.OwnerField); owner == "" || owner != subjectID {
			return &Refusal{}, nil
		}
	}

	if sc := p.Scope; sc != nil && !containsFold(sc.BypassRoles, role) {
		resolved, err := find(sc.ResolveCollection, Match{sc.ResolveKeyField, id})
		if err != nil {
			return nil, fmt.Errorf("resolving scope: %w", err)
		}
		if resolved == nil {
			return &Refusal{Message: sc.Message}, nil
		}
		member, err := find(sc.MemberOfCollection,
			Match{sc.MemberOfMatchField, resolved.GetString(sc.ResolveSelectField)},
			Match{sc.MemberOfActorField, subjectID})
		if err != nil {
			return nil, fmt.Errorf("resolving scope membership: %w", err)
		}
		if member == nil {
			return &Refusal{Message: sc.Message}, nil
		}
	}
	return nil, nil
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
