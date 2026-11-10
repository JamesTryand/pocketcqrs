package cmdauth

import (
	"reflect"
	"testing"
)

func TestLiveZeroValueAllowsEverything(t *testing.T) {
	var l Live
	if got := l.Policies(); got == nil || len(got) != 0 {
		t.Fatalf("zero Live should be an empty table, got %#v", got)
	}
	l.Set(nil)
	if got := l.Policies(); got == nil || len(got) != 0 {
		t.Fatalf("Set(nil) should be an empty table, got %#v", got)
	}
}

func TestLiveSwaps(t *testing.T) {
	var l Live
	k := Key{Aggregate: "ticket", Command: "OpenTicket"}
	l.Set(map[Key]Policy{k: {RequiredRole: []string{"operator"}}})
	if _, ok := l.Policies()[k]; !ok {
		t.Fatal("policy missing after Set")
	}
	l.Set(map[Key]Policy{})
	if _, ok := l.Policies()[k]; ok {
		t.Fatal("policy still there after a swap that removed it")
	}
}

func TestPolicyEncodeDecodeRoundTrips(t *testing.T) {
	p := Policy{
		RequiredRole:   []string{"supervisor", "admin"},
		FieldGatedRole: &FieldGatedRolePolicy{Field: "priority", Value: "urgent", RequiredRole: []string{"supervisor"}},
		Ownership:      &OwnershipPolicy{Collection: "tickets", KeyField: "ticketId", OwnerField: "agentId", BypassRoles: []string{"admin"}},
		Scope: &ScopePolicy{ResolveCollection: "tickets", ResolveKeyField: "ticketId", ResolveSelectField: "teamId",
			MemberOfCollection: "teamMembers", MemberOfMatchField: "teamId", MemberOfActorField: "memberId",
			BypassRoles: []string{"admin"}, Message: "only the team"},
	}
	raw, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodePolicy(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, p) {
		t.Fatalf("round trip differs:\n got  %#v\n want %#v", back, p)
	}
}

func TestDeclares(t *testing.T) {
	if (Policy{}).Declares() {
		t.Fatal("an empty policy declares nothing")
	}
	if !(Policy{RequiredRole: []string{"x"}}).Declares() {
		t.Fatal("a required role is a declaration")
	}
}
