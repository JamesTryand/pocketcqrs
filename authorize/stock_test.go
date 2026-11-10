package authorize

import (
	"errors"
	"testing"

	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/cmdauth"
	"github.com/jamestryand/pocketcqrs/gateway"
	"github.com/jamestryand/pocketcqrs/pii"
)

func caller(collection, role string) *core.Record {
	c := core.NewAuthCollection(collection)
	c.Fields.Add(&core.TextField{Name: "role"})
	r := core.NewRecord(c)
	r.Id = "caller1"
	r.Set("role", role)
	return r
}

func stockHook(policies map[cmdauth.Key]cmdauth.Policy) func(re *core.RequestEvent, aggregate, command, id string, payload []byte) (bool, error) {
	live := &cmdauth.Live{}
	live.Set(policies)
	return Stock(live, "", "")
}

var openNeedsOperator = map[cmdauth.Key]cmdauth.Policy{
	{Aggregate: "ticket", Command: "OpenTicket"}: {RequiredRole: []string{"operator"}},
}

func TestStockAllowsWhatNoPolicyNames(t *testing.T) {
	hook := stockHook(openNeedsOperator)
	ok, err := hook(&core.RequestEvent{Auth: caller("users", "")}, "ticket", "CloseTicket", "t1", nil)
	if !ok || err != nil {
		t.Fatalf("an undeclared command must be allowed, got %v %v", ok, err)
	}
	ok, err = stockHook(nil)(&core.RequestEvent{Auth: caller("users", "")}, "ticket", "OpenTicket", "t1", nil)
	if !ok || err != nil {
		t.Fatalf("with no policies at all every command is allowed, got %v %v", ok, err)
	}
}

func TestStockEnforcesARequiredRole(t *testing.T) {
	hook := stockHook(openNeedsOperator)
	if ok, err := hook(&core.RequestEvent{Auth: caller("users", "")}, "ticket", "OpenTicket", "t1", nil); ok || err != nil {
		t.Fatalf("no role: want refused (false, nil), got %v %v", ok, err)
	}
	if ok, err := hook(&core.RequestEvent{Auth: caller("users", "operator")}, "ticket", "OpenTicket", "t1", nil); !ok || err != nil {
		t.Fatalf("operator: want allowed, got %v %v", ok, err)
	}
	if ok, _ := hook(&core.RequestEvent{}, "ticket", "OpenTicket", "t1", nil); ok {
		t.Fatal("an anonymous caller holds no role, so it must be refused")
	}
}

// Spelling the command or aggregate in another case meets the same policy.
func TestStockIgnoresSpelling(t *testing.T) {
	hook := stockHook(openNeedsOperator)
	for _, c := range [][2]string{{"ticket", "openticket"}, {"Ticket", "OpenTicket"}, {"TICKET", "OPENTICKET"}} {
		if ok, _ := hook(&core.RequestEvent{Auth: caller("users", "")}, c[0], c[1], "t1", nil); ok {
			t.Fatalf("%s/%s without the role was allowed", c[0], c[1])
		}
	}
}

func TestStockLetsASuperuserThrough(t *testing.T) {
	hook := stockHook(openNeedsOperator)
	su := core.NewRecord(core.NewAuthCollection(core.CollectionNameSuperusers))
	if ok, err := hook(&core.RequestEvent{Auth: su}, "ticket", "OpenTicket", "t1", nil); !ok || err != nil {
		t.Fatalf("a superuser passes, got %v %v", ok, err)
	}
}

// The stock hook keeps the refusal the gateway gives dataSubject commands
// with no hook, for everyone, superusers included.
func TestStockKeepsErasureRefused(t *testing.T) {
	hook := stockHook(nil)
	su := core.NewRecord(core.NewAuthCollection(core.CollectionNameSuperusers))
	ok, err := hook(&core.RequestEvent{Auth: su}, pii.SubjectAggregate, pii.RequestErasureCommand, "s1", nil)
	var refusal *cmdauth.Refusal
	if ok || !errors.As(err, &refusal) || refusal.Message != gateway.ErasureRefused {
		t.Fatalf("want the erasure refusal, got %v %v", ok, err)
	}
}

func TestStockReadsTheConfiguredRoleField(t *testing.T) {
	live := &cmdauth.Live{}
	live.Set(openNeedsOperator)
	hook := Stock(live, "jobRole", "")
	c := core.NewAuthCollection("users")
	c.Fields.Add(&core.TextField{Name: "jobRole"})
	r := core.NewRecord(c)
	r.Set("jobRole", "operator")
	if ok, err := hook(&core.RequestEvent{Auth: r}, "ticket", "OpenTicket", "t1", nil); !ok || err != nil {
		t.Fatalf("role read from --cqrsRoleField: want allowed, got %v %v", ok, err)
	}
}
