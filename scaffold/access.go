package scaffold

import (
	"fmt"
	"strings"
)

// SelfAccess is a read model's selfAccess (schema 3.2.0; param 3.8.0): a
// caller who doesn't hold its RequiredRole sees the rows whose
// SubjectField is their own data-subject id.
type SelfAccess struct {
	// SubjectField is the read model's own column naming each row's data
	// subject.
	SubjectField string `json:"subjectField"`
	// Param, when a request carries it, narrows any caller (role holders
	// too) to their own rows. It only ever narrows, so on the JS target it
	// is the client's filter (`<SubjectField> = @request.auth.<field>`),
	// never part of the rule.
	Param string `json:"param,omitempty"`
}

// HasAccessRules reports whether rm declares who may read it: a required
// role, self access, or a scope that grants access.
func (rm ReadModel) HasAccessRules() bool {
	if len(rm.RequiredRole) > 0 || rm.SelfAccess != nil {
		return true
	}
	for _, sc := range rm.Scopes {
		if sc.GrantsAccess {
			return true
		}
	}
	return false
}

// authField resolves an auth-record field name, falling back to def.
func authField(name, def string) string {
	if name == "" {
		return def
	}
	return name
}

// readRule is the PocketBase List/View rule for rm on the JS target, the
// whole access rule of schema 3.8.0, stated in PocketBase's own rule
// language so the record API (lists, views, realtime) enforces it:
//
//   - a caller holding RequiredRole sees every row;
//   - any other signed-in caller sees the rows whose SelfAccess subject is
//     their own subject id, and the rows each granting scope admits for
//     them: a row of the scope's read model matching their subject id and
//     selecting this row's value;
//   - a read model with none of these is readable by every signed-in
//     caller when the document said nothing (SignedInByDefault), and
//     otherwise has no rule ("" with ok false: the deployment default).
//
// The caller's role and subject id are fields of their auth record
// (d.RoleField, default "role"; d.SubjectField, default the record id),
// which core's users collection refuses to let a user set on themselves.
// An empty subject id never matches, so a caller without one sees only
// what a role gives them.
func (d Domain) readRule(rm ReadModel) (rule string, ok bool) {
	if !rm.HasAccessRules() {
		if d.SignedInByDefault {
			return "authenticated", true
		}
		return "", false
	}
	role := "@request.auth." + authField(d.RoleField, "role")
	subject := "@request.auth." + authField(d.SubjectField, "id")

	var clauses []string
	for _, r := range rm.RequiredRole {
		clauses = append(clauses, fmt.Sprintf("%s = %q", role, r))
	}
	if sa := rm.SelfAccess; sa != nil {
		clauses = append(clauses, fmt.Sprintf(`(%s != "" && %s = %s)`, subject, sa.SubjectField, subject))
	}
	for i, sc := range rm.Scopes {
		if !sc.GrantsAccess {
			continue
		}
		// PocketBase joins each @collection reference once, so both
		// conditions hold on the SAME row of the scope's read model; the
		// alias keeps two granting scopes through one read model
		// independent of each other
		via := fmt.Sprintf("@collection.%s:grant%d", sc.Via.Collection, i)
		clauses = append(clauses, fmt.Sprintf(`(%s != "" && %s.%s ?= %s && %s.%s ?= %s)`,
			subject, via, sc.Via.MatchParamTo, subject, via, sc.Via.SelectField, sc.Via.FilterLocalField))
	}
	// every clause fails for a signed-out caller (no role, empty subject)
	return strings.Join(clauses, " || "), true
}

func hasGrant(rm ReadModel) bool {
	for _, sc := range rm.Scopes {
		if sc.GrantsAccess {
			return true
		}
	}
	return false
}
