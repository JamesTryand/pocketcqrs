package authorize

import (
	"testing"

	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/cmdauth"
	"github.com/jamestryand/pocketcqrs/pii"
)

var captureNeedsIntake = map[cmdauth.Key]cmdauth.Policy{
	{Aggregate: "contact", Command: "CaptureContact"}: {RequiredRole: []string{"operator", "intake"}},
}

// A compiled-in domain's policies hold beside the JS deciders', and a reload
// (Live.Set with the JS table alone) does not drop them.
func TestStockWithKeepsFixedPoliciesAcrossAReload(t *testing.T) {
	live := &cmdauth.Live{}
	live.Set(openNeedsOperator)
	hook := StockWith(StockConfig{Live: live, Fixed: captureNeedsIntake})
	check := func(when string) {
		t.Helper()
		if ok, _ := hook(&core.RequestEvent{Auth: caller("users", "period-check")}, "contact", "CaptureContact", "c1", nil); ok {
			t.Fatalf("%s: a fixed policy let the wrong role through", when)
		}
		if ok, err := hook(&core.RequestEvent{Auth: caller("users", "intake")}, "contact", "CaptureContact", "c1", nil); !ok || err != nil {
			t.Fatalf("%s: the declared role was refused: %v %v", when, ok, err)
		}
		if ok, _ := hook(&core.RequestEvent{Auth: caller("users", "")}, "contact", "captureContact", "c1", nil); ok {
			t.Fatalf("%s: another spelling got past a fixed policy", when)
		}
		if ok, err := hook(&core.RequestEvent{Auth: caller("users", "")}, "contact", "Undeclared", "c1", nil); !ok || err != nil {
			t.Fatalf("%s: an undeclared command must stay allowed: %v %v", when, ok, err)
		}
	}
	check("before a reload")
	if ok, _ := hook(&core.RequestEvent{Auth: caller("users", "")}, "ticket", "OpenTicket", "t1", nil); ok {
		t.Fatal("the JS table is enforced too")
	}
	live.Set(map[cmdauth.Key]cmdauth.Policy{}) // a reload whose deciders declare nothing
	check("after a reload")
	if ok, _ := hook(&core.RequestEvent{Auth: caller("users", "")}, "ticket", "OpenTicket", "t1", nil); !ok {
		t.Fatal("the reloaded JS table no longer names OpenTicket, so it is allowed")
	}
}

// With no erasure wiring, StockWith is Stock: every dataSubject command is
// refused, superusers included.
func TestStockWithWithoutErasureKeepsTheStockRefusal(t *testing.T) {
	hook := StockWith(StockConfig{Live: &cmdauth.Live{}, Fixed: captureNeedsIntake})
	su := core.NewRecord(core.NewAuthCollection(core.CollectionNameSuperusers))
	for _, cmd := range []string{pii.RequestErasureCommand, pii.EraseSubjectCommand} {
		if ok, _ := hook(&core.RequestEvent{Auth: su}, pii.SubjectAggregate, cmd, "s1", nil); ok {
			t.Fatalf("%s was allowed with no erasure wiring", cmd)
		}
	}
}

func TestStockWithErasureAsTheHostDecides(t *testing.T) {
	var asked []string
	hook := StockWith(StockConfig{
		Live:           &cmdauth.Live{},
		ErasureRequest: &cmdauth.ErasureRequestPolicy{Roles: []string{"operator"}},
		SubjectCommand: func(re *core.RequestEvent, command, id string, payload []byte) (bool, error) {
			asked = append(asked, command)
			return re.Auth != nil && re.Auth.GetString("role") == "operator", nil
		},
	})
	req := func(role, cmd string) bool {
		ok, _ := hook(&core.RequestEvent{Auth: caller("users", role)}, pii.SubjectAggregate, cmd, "s1", nil)
		return ok
	}
	if !req("operator", pii.RequestErasureCommand) {
		t.Fatal("the declared role may request erasure")
	}
	if req("intake", pii.RequestErasureCommand) {
		t.Fatal("a role the declaration does not name may not request erasure")
	}
	if !req("operator", pii.EraseSubjectCommand) || req("intake", pii.EraseSubjectCommand) {
		t.Fatal("EraseSubject is the host's SubjectCommand to decide")
	}
	if len(asked) != 2 {
		t.Fatalf("SubjectCommand decides only the other commands, asked %v", asked)
	}
	su := core.NewRecord(core.NewAuthCollection(core.CollectionNameSuperusers))
	if ok, _ := hook(&core.RequestEvent{Auth: su}, pii.SubjectAggregate, pii.EraseSubjectCommand, "s1", nil); !ok {
		t.Fatal("with erasure wired, a superuser passes")
	}

	noOthers := StockWith(StockConfig{Live: &cmdauth.Live{}, ErasureRequest: &cmdauth.ErasureRequestPolicy{Roles: []string{"operator"}}})
	if ok, _ := noOthers(&core.RequestEvent{Auth: caller("users", "operator")}, pii.SubjectAggregate, pii.EraseSubjectCommand, "s1", nil); ok {
		t.Fatal("with no SubjectCommand, the other dataSubject commands stay refused")
	}
}
