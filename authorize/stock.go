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
	return StockWith(StockConfig{Live: live, RoleField: roleField, SubjectField: subjectField})
}

// StockConfig is what StockWith enforces. Its zero value except Live is
// Stock.
type StockConfig struct {
	// Live is the JS deciders' table, swapped on every reload.
	Live *cmdauth.Live
	// Fixed is compiled-in domains' table (host.Host.DeclareCommands), never
	// swapped: a hot reload replaces only Live.
	Fixed map[Key]Policy
	// RoleField and SubjectField: see Stock.
	RoleField, SubjectField string
	// ErasureRequest admits RequestErasure as a document's
	// dataSubjects.erasure declares (BuildErasureRequest).
	ErasureRequest *cmdauth.ErasureRequestPolicy
	// SubjectCommand decides the dataSubject aggregate's other commands
	// (EraseSubject, a module's holds and approvals).
	SubjectCommand func(re *core.RequestEvent, command, id string, payload []byte) (bool, error)
}

// StockWith is Stock for a host that adds compiled-in domains: their policies
// beside the JS deciders', and erasure as the host decides.
//
// With neither ErasureRequest nor SubjectCommand set, every dataSubject
// command keeps the stock refusal (gateway.ErasureRefused), superusers
// included. With either set, a superuser passes, RequestErasure is admitted
// as ErasureRequest says (nil admits no one), and any other dataSubject
// command is SubjectCommand's to decide (nil refuses it).
func StockWith(cfg StockConfig) func(re *core.RequestEvent, aggregate, command, id string, payload []byte) (bool, error) {
	roleField := cfg.RoleField
	if roleField == "" {
		roleField = "role"
	}
	subjectField := cfg.SubjectField
	erasureWired := cfg.ErasureRequest != nil || cfg.SubjectCommand != nil
	ownRole := func(auth *core.Record) string {
		if auth == nil {
			return ""
		}
		return auth.GetString(roleField)
	}
	ownSubjectID := func(auth *core.Record) string {
		if auth == nil {
			return ""
		}
		if subjectField != "" {
			return auth.GetString(subjectField)
		}
		return auth.Id
	}
	return func(re *core.RequestEvent, aggregate, command, id string, payload []byte) (bool, error) {
		superuser := re.Auth != nil && re.Auth.IsSuperuser()
		if strings.EqualFold(aggregate, pii.SubjectAggregate) {
			if !erasureWired {
				return false, &cmdauth.Refusal{Message: gateway.ErasureRefused}
			}
			if superuser {
				return true, nil
			}
			if command != pii.RequestErasureCommand {
				if cfg.SubjectCommand == nil {
					return false, nil
				}
				return cfg.SubjectCommand(re, command, id, payload)
			}
			a := &Authorizer{ErasureRequest: cfg.ErasureRequest, ResolveOwnRole: ownRole, ResolveOwnSubjectID: ownSubjectID}
			return a.Authorize(re.App, pii.SubjectAggregate, command, id, payload, re.Auth)
		}
		if superuser {
			return true, nil
		}
		policies := map[Key]Policy{}
		if cfg.Live != nil {
			policies = cfg.Live.Policies()
		}
		if len(cfg.Fixed) > 0 {
			merged := make(map[Key]Policy, len(policies)+len(cfg.Fixed))
			for k, v := range policies {
				merged[k] = v
			}
			for k, v := range cfg.Fixed {
				merged[k] = v
			}
			policies = merged
		}
		// The policy is found whatever the spelling: a decider decides what
		// a command name means, and one that reads it loosely must not let
		// "openticket" past the check "OpenTicket" declares.
		aggregate, command = canonical(policies, aggregate, command)
		a := &Authorizer{Policies: policies, ResolveOwnRole: ownRole, ResolveOwnSubjectID: ownSubjectID}
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
