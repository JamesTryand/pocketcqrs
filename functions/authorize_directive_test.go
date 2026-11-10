package functions

import (
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/cmdauth"
)

const authorizeDecider = `//@trigger decider ticket
//@handles TicketOpened
//@commands OpenTicket CloseTicket
//@authorize OpenTicket {"RequiredRole":["operator"]}
function initialState() { return {}; }
function decide(command, state) { return [{ type: 'TicketOpened', data: {} }]; }
function evolve(state, event) { return state; }
`

func TestAuthorizeDirectiveLoads(t *testing.T) {
	dir := t.TempDir()
	writeFn(t, dir, "ticket.js", authorizeDecider)
	loaded, err := LoadDir(NewGojaRuntime(nil), nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	specs := map[string]*DeciderSpec{}
	for _, s := range loaded.Deciders {
		specs[s.Aggregate] = s
	}
	table := CommandPolicies(specs)
	p, ok := table[cmdauth.Key{Aggregate: "ticket", Command: "OpenTicket"}]
	if !ok || len(p.RequiredRole) != 1 || p.RequiredRole[0] != "operator" {
		t.Fatalf("want OpenTicket requiring operator, got %#v", table)
	}
	if _, ok := table[cmdauth.Key{Aggregate: "ticket", Command: "CloseTicket"}]; ok {
		t.Fatalf("CloseTicket declares nothing, so it has no policy: %#v", table)
	}
}

// A decider with no //@authorize loads to an empty table: nothing checked.
func TestNoAuthorizeDirectiveMeansEmptyTable(t *testing.T) {
	dir := t.TempDir()
	writeFn(t, dir, "ticket.js", strings.Replace(authorizeDecider,
		"//@authorize OpenTicket {\"RequiredRole\":[\"operator\"]}\n", "", 1))
	loaded, err := LoadDir(NewGojaRuntime(nil), nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := CommandPolicies(map[string]*DeciderSpec{"ticket": loaded.Deciders[0]}); len(got) != 0 {
		t.Fatalf("want an empty table, got %#v", got)
	}
}

// Every malformed //@authorize is refused, never dropped: a dropped line is
// a check the deployment believes it has.
func TestAuthorizeDirectiveRefusals(t *testing.T) {
	cases := map[string]struct{ line, want string }{
		"no policy":       {`//@authorize OpenTicket`, "wants: authorize"},
		"bad json":        {`//@authorize OpenTicket {"RequiredRole":`, "//@authorize OpenTicket"},
		"unknown key":     {`//@authorize OpenTicket {"RequiredRoles":["operator"]}`, "unknown field"},
		"declares none":   {`//@authorize OpenTicket {}`, "declares no requirement"},
		"bad name":        {`//@authorize open-ticket {"RequiredRole":["operator"]}`, "not a command name"},
		"unknown command": {`//@authorize ReopenTicket {"RequiredRole":["operator"]}`, "not in //@commands"},
		"duplicate": {`//@authorize OpenTicket {"RequiredRole":["operator"]}
//@authorize OpenTicket {"RequiredRole":["admin"]}`, "declared twice"},
		"half a scope": {`//@authorize OpenTicket {"Scope":{"ResolveCollection":"tickets"}}`, "Scope needs"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFn(t, dir, "ticket.js", strings.Replace(authorizeDecider,
				"//@authorize OpenTicket {\"RequiredRole\":[\"operator\"]}", c.line, 1))
			_, err := LoadDir(NewGojaRuntime(nil), nil, dir)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestAuthorizeDirectiveNeedsADecider(t *testing.T) {
	dir := t.TempDir()
	writeFn(t, dir, "audit.js", "//@trigger event TicketOpened\n//@authorize OpenTicket {\"RequiredRole\":[\"operator\"]}\nconsole.log(1);\n")
	_, err := LoadDir(NewGojaRuntime(nil), nil, dir)
	if err == nil || !strings.Contains(err.Error(), "requires //@trigger decider") {
		t.Fatalf("want a refusal, got %v", err)
	}
}
