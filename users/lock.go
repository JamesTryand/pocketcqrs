package users

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

var fieldName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// LockFields makes the users collection's self-service rules refuse a
// request that sets any of fields: its CreateRule (open registration) and
// its UpdateRule (a user editing their own record) gain
// `@request.body.<field>:isset = false` for each.
//
// The fields are the ones read models' access rules read from the caller's
// auth record: their role and their data-subject id (schema import's
// --roleField/--subjectField, the server's --cqrsRoleField/
// --cqrsSubjectField). Left self-service, a user could register with, or
// promote themselves to, any role and read whatever it reads. A superuser,
// or a command handled server-side, still sets them.
//
// The app's own rule is kept and ANDed with the lock; a superuser-only rule
// (nil) is already locked and stays nil. Idempotent, so it runs on every
// boot and also locks deployments made before it existed.
func LockFields(app core.App, fields ...string) error {
	col, err := app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		return nil // no users collection: nothing to lock
	}
	var clauses []string
	for _, f := range fields {
		if f == "" || f == "id" {
			continue // the record id is never writable
		}
		if !fieldName.MatchString(f) {
			return fmt.Errorf("users: %q is not a field name", f)
		}
		clauses = append(clauses, "@request.body."+f+":isset = false")
	}
	if len(clauses) == 0 {
		return nil
	}
	changed := false
	lock := func(rule *string) *string {
		if rule == nil {
			return nil
		}
		var missing []string
		for _, c := range clauses {
			if !strings.Contains(*rule, c) {
				missing = append(missing, c)
			}
		}
		if len(missing) == 0 {
			return rule
		}
		changed = true
		out := strings.Join(missing, " && ")
		if strings.TrimSpace(*rule) != "" {
			out = "(" + *rule + ") && " + out
		}
		return &out
	}
	col.CreateRule = lock(col.CreateRule)
	col.UpdateRule = lock(col.UpdateRule)
	if !changed {
		return nil
	}
	return app.Save(col)
}
