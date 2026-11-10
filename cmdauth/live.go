package cmdauth

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
)

// Live is a policy table a running server checks while a hot reload may
// replace it. The stock server builds it from its JS deciders' //@authorize
// lines at boot and swaps it whenever deciders swap, so a command's declared
// authorization changes with the code that decides it, never separately.
// The zero value is an empty table: every command allowed.
type Live struct {
	table atomic.Pointer[map[Key]Policy]
}

// Set replaces the table. Nil or empty means no command has a policy.
func (l *Live) Set(policies map[Key]Policy) {
	if policies == nil {
		policies = map[Key]Policy{}
	}
	l.table.Store(&policies)
}

// Policies is the current table, never nil.
func (l *Live) Policies() map[Key]Policy {
	if p := l.table.Load(); p != nil {
		return *p
	}
	return map[Key]Policy{}
}

// Declares reports whether p requires anything at all. A policy that
// declares nothing is not a policy: carrying one would read as a check that
// isn't there.
func (p Policy) Declares() bool {
	return len(p.RequiredRole) > 0 || p.FieldGatedRole != nil || p.Ownership != nil || p.Scope != nil
}

// Encode is p as the JSON an //@authorize directive carries.
func (p Policy) Encode() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DecodePolicy reads the JSON of an //@authorize directive strictly: an
// unknown key is refused, never dropped, because a dropped key is a check a
// deployment believes it has and doesn't.
func DecodePolicy(raw string) (Policy, error) {
	var p Policy
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, err
	}
	if dec.More() {
		return Policy{}, fmt.Errorf("trailing text after the policy")
	}
	if !p.Declares() {
		return Policy{}, fmt.Errorf("the policy declares no requirement")
	}
	if fg := p.FieldGatedRole; fg != nil && (fg.Field == "" || len(fg.RequiredRole) == 0) {
		return Policy{}, fmt.Errorf("FieldGatedRole needs Field and RequiredRole")
	}
	if o := p.Ownership; o != nil && (o.Collection == "" || o.KeyField == "" || o.OwnerField == "") {
		return Policy{}, fmt.Errorf("Ownership needs Collection, KeyField and OwnerField")
	}
	if s := p.Scope; s != nil && (s.ResolveCollection == "" || s.ResolveKeyField == "" || s.ResolveSelectField == "" ||
		s.MemberOfCollection == "" || s.MemberOfMatchField == "" || s.MemberOfActorField == "") {
		return Policy{}, fmt.Errorf("Scope needs every collection and field named")
	}
	return p, nil
}
