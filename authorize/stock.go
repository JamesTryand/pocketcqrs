package authorize

import (
	"strings"

	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/cmdauth"
	"github.com/jamestryand/pocketcqrs/gateway"
	"github.com/jamestryand/pocketcqrs/pii"
)

// Stock is the stock server's gateway.Config.Authorize: it enforces the
// policies the active JS deciders declare (//@authorize, built by
// functions.CommandPolicies into live), so a model's command permissions
// hold on a deployment that wires nothing of its own.
//
// It changes nothing for a command no policy names: that command is allowed,
// exactly as with no hook at all. In particular:
//   - a superuser passes every check, as a superuser passes every PocketBase
//     rule (the operator, the admin UI and scripts keep working);
//   - the caller's role is its auth record's roleField and its own subject id
//     is subjectField, or the record id when subjectField is "" (the same
//     fields read models' //@rule reads, --cqrsRoleField/--cqrsSubjectField);
//   - an anonymous caller (--cqrsAllowAnonymous) holds no role, so a command
//     that requires one refuses it;
//   - the built-in dataSubject aggregate keeps the refusal the gateway gives
//     with no hook (gateway.ErasureRefused): who may erase whom is a host's
//     decision, wired in its own Authorize, never the stock server's.
func Stock(live *cmdauth.Live, roleField, subjectField string) func(re *core.RequestEvent, aggregate, command, id string, payload []byte) (bool, error) {
	if roleField == "" {
		roleField = "role"
	}
	return func(re *core.RequestEvent, aggregate, command, id string, payload []byte) (bool, error) {
		if strings.EqualFold(aggregate, pii.SubjectAggregate) {
			return false, &cmdauth.Refusal{Message: gateway.ErasureRefused}
		}
		if re.Auth != nil && re.Auth.IsSuperuser() {
			return true, nil
		}
		policies := live.Policies()
		// The policy is found whatever the spelling: a decider decides what
		// a command name means, and one that reads it loosely must not let
		// "openticket" past the check "OpenTicket" declares.
		aggregate, command = canonical(policies, aggregate, command)
		a := &Authorizer{
			Policies: policies,
			ResolveOwnRole: func(auth *core.Record) string {
				if auth == nil {
					return ""
				}
				return auth.GetString(roleField)
			},
			ResolveOwnSubjectID: func(auth *core.Record) string {
				if auth == nil {
					return ""
				}
				if subjectField != "" {
					return auth.GetString(subjectField)
				}
				return auth.Id
			},
		}
		return a.Authorize(re.App, aggregate, command, id, payload, re.Auth)
	}
}

// canonical is the table's own spelling of (aggregate, command) when a
// policy names it in any case, or the request's spelling when none does.
func canonical(policies map[Key]Policy, aggregate, command string) (string, string) {
	if _, ok := policies[Key{Aggregate: aggregate, Command: command}]; ok {
		return aggregate, command
	}
	for k := range policies {
		if strings.EqualFold(k.Aggregate, aggregate) && strings.EqualFold(k.Command, command) {
			return k.Aggregate, k.Command
		}
	}
	return aggregate, command
}
