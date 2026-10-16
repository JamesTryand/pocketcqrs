package emschema

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pocketbase/pocketbase/core"
	// PocketBase's own system migrations (collections, superusers): a bare
	// BaseApp has no tables without them
	_ "github.com/pocketbase/pocketbase/migrations"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/jamestryand/pocketcqrs/scaffold"
)

// ScenarioCaller is a stateView scenario's when.caller (schema 3.8.0): who
// the query reads as. Without one, the reader holds the read model's
// requiredRole.
type ScenarioCaller struct {
	SubjectID string `json:"subjectId"`
	Role      string `json:"role,omitempty"`
}

var ruleDirective = regexp.MustCompile(`(?m)^//@rule (\S+) (.+)$`)

// verifySubjectField stands in for the record id as the caller's subject
// when the import reads it from there (--subjectField empty): a scenario's
// subject ids ("member-1") are not valid PocketBase record ids.
const verifySubjectField = "verifySubject"

// visibleTo keeps the rows of collection that caller may read, by running
// the read rule the generator emitted (its //@rule) in a scratch PocketBase
// app: the rule that will actually be enforced, @collection joins and all,
// not a second implementation of it. The
// collections a granting scope reads through are built from the same
// `given` events and loaded beside it.
func (v *verifier) visibleTo(caller ScenarioCaller, collection string, rows map[string]map[string]any,
	given []EventRef, sharedDefault string, scenarioID string) (map[string]map[string]any, string) {
	rule, ok := v.ruleOf(collection)
	if !ok {
		return nil, fmt.Sprintf("no read rule was generated for %q", collection)
	}
	// every collection the rule may join, with its own rows
	tables := map[string]map[string]map[string]any{collection: rows}
	for _, name := range collectionsIn(rule) {
		if name == collection {
			continue
		}
		joinedRows, detail := v.projectRows(name, given, sharedDefault, scenarioID)
		if detail != "" {
			return nil, detail
		}
		tables[name] = joinedRows
	}
	if v.collecting() {
		return rows, ""
	}
	dir, err := os.MkdirTemp(v.dir, "rules-")
	if err != nil {
		return nil, err.Error()
	}
	defer os.RemoveAll(dir)
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: filepath.Join(dir, "pb_data")})
	defer app.ResetBootstrapState() // closes the databases, so the directory can go
	if err := app.Bootstrap(); err != nil {
		return nil, "the rule sandbox did not start: " + err.Error()
	}

	// every collection first: a rule naming another (@collection) is only
	// accepted once that one exists
	cols := map[string]*core.Collection{}
	for name, table := range tables {
		col := core.NewBaseCollection(name)
		col.Fields.Add(&core.TextField{Name: "rowKey"})
		for _, f := range v.fieldsOf(name, table) {
			col.Fields.Add(f)
		}
		if err := app.Save(col); err != nil {
			return nil, fmt.Sprintf("the rule sandbox could not create %q: %v", name, err)
		}
		cols[name] = col
	}
	records := map[string]*core.Record{}
	for name, table := range tables {
		col := cols[name]
		r, _ := v.ruleOf(name)
		col.ListRule, col.ViewRule = types.Pointer(r), types.Pointer(r)
		if err := app.Save(col); err != nil {
			return nil, fmt.Sprintf("the rule sandbox refused %q's generated rule %q: %v", name, r, err)
		}
		for key, row := range table {
			rec := core.NewRecord(col)
			rec.Set("rowKey", key)
			for field, val := range row {
				if col.Fields.GetByName(field) != nil {
					rec.Set(field, val)
				}
			}
			if err := app.Save(rec); err != nil {
				return nil, fmt.Sprintf("loading %s row %q into the rule sandbox: %v", name, key, err)
			}
			if name == collection {
				records[key] = rec
			}
		}
	}

	roleField, subjectField := v.callerFields()
	if subjectField == "id" {
		subjectField = verifySubjectField
		if rule != `@request.auth.id != ""` {
			rule = strings.ReplaceAll(rule, "@request.auth.id", "@request.auth."+verifySubjectField)
		}
	}
	callers := core.NewAuthCollection("verify_callers")
	callers.Fields.Add(&core.TextField{Name: roleField}, &core.TextField{Name: subjectField})
	if err := app.Save(callers); err != nil {
		return nil, "the rule sandbox could not hold the caller: " + err.Error()
	}
	who := core.NewRecord(callers)
	who.SetEmail("caller@verify.local")
	who.SetPassword("verify-caller-password")
	who.Set(roleField, caller.Role)
	who.Set(subjectField, caller.SubjectID)
	if err := app.Save(who); err != nil {
		return nil, "the rule sandbox could not hold the caller: " + err.Error()
	}

	info := &core.RequestInfo{Auth: who, Method: http.MethodGet, Context: core.RequestInfoContextDefault}
	visible := map[string]map[string]any{}
	for key, rec := range records {
		ok, err := app.CanAccessRecord(rec, info, &rule)
		if err != nil {
			return nil, fmt.Sprintf("the generated rule for %q failed in PocketBase: %v (rule: %s)", collection, err, rule)
		}
		if ok {
			visible[key] = rows[key]
		}
	}
	return visible, ""
}

// ruleOf is the read rule the generated projection for collection
// declares, resolved as the server resolves it.
func (v *verifier) ruleOf(collection string) (string, bool) {
	file, ok := v.sources[collection+".js"]
	if !ok {
		return "", false
	}
	m := ruleDirective.FindStringSubmatch(file.Source)
	if m == nil {
		return "", false
	}
	switch m[2] {
	case "public":
		return "", true
	case "authenticated":
		return `@request.auth.id != ""`, true
	}
	return m[2], true
}

var collectionRef = regexp.MustCompile(`@collection\.([A-Za-z_][A-Za-z0-9_]*)`)

// collectionsIn lists the collections a rule joins.
func collectionsIn(rule string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range collectionRef.FindAllStringSubmatch(rule, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// projectRows folds the given events through the generated projection for
// collection, as the queried read model's own rows are.
func (v *verifier) projectRows(collection string, given []EventRef, sharedDefault, scenarioID string) (map[string]map[string]any, string) {
	var rm ReadModel
	for id, candidate := range v.doc.ReadModels {
		if scaffold.SanitizeName(ReadModelCollectionName(candidate.Name, id)) == collection {
			rm = candidate
		}
	}
	rows, problem, missing := v.project(scenarioID, collection, v.buildFixture(given, idAttributeField(rm), sharedDefault))
	switch {
	case missing:
		return nil, fmt.Sprintf("the rule joins %q, which was not generated", collection)
	case problem != "":
		return nil, fmt.Sprintf("the projection for %q %s", collection, problem)
	}
	return rows, ""
}

// fieldsOf is a sandbox column per generated field of collection, typed as
// the server types it, plus any other column its rows carry.
func (v *verifier) fieldsOf(collection string, rows map[string]map[string]any) []core.Field {
	var out []core.Field
	seen := map[string]bool{"rowKey": true}
	add := func(name, typ string) {
		if seen[name] {
			return
		}
		seen[name] = true
		switch typ {
		case "number":
			out = append(out, &core.NumberField{Name: name})
		case "bool":
			out = append(out, &core.BoolField{Name: name})
		case "json":
			out = append(out, &core.JSONField{Name: name})
		default:
			out = append(out, &core.TextField{Name: name})
		}
	}
	if rm, ok := v.readModels[collection]; ok {
		add(rm.Key, "text")
		for _, f := range rm.Fields {
			add(f.Name, f.Type)
		}
	}
	for _, row := range rows {
		for name, val := range row {
			switch val.(type) {
			case float64, json.Number:
				add(name, "number")
			case bool:
				add(name, "bool")
			case map[string]any, []any:
				add(name, "json")
			default:
				add(name, "text")
			}
		}
	}
	return out
}

// bindCaller applies what a caller's own identity does to the query
// params: a granting scope's param always names the caller, whatever value
// it was given (3.8.0), and selfAccess.param narrows the caller, role
// holders too, to their own rows. A scenario without a caller reads as a
// role holder, and the value it sends names the subject (dotnetcqrs's
// harness binds it the same way).
func bindCaller(rm ReadModel, rows map[string]map[string]any, queryParams json.RawMessage, caller *ScenarioCaller) (map[string]map[string]any, json.RawMessage) {
	var params map[string]any
	if len(queryParams) == 0 || json.Unmarshal(queryParams, &params) != nil || params == nil {
		return rows, queryParams
	}
	if caller != nil {
		for _, sc := range rm.Scopes {
			if _, ok := params[sc.Param]; ok && sc.GrantsAccess {
				params[sc.Param] = caller.SubjectID
			}
		}
	}
	if sa := rm.SelfAccess; sa != nil && sa.Param != "" {
		if sent, ok := params[sa.Param]; ok {
			delete(params, sa.Param)
			subject := sent
			if caller != nil {
				subject = caller.SubjectID
			}
			own := map[string]map[string]any{}
			if subject != nil && subject != "" {
				for key, row := range rows {
					if jsonEq(row[scaffold.SanitizeName(sa.SubjectField)], subject) {
						own[key] = row
					}
				}
			}
			rows = own
		}
	}
	out, _ := json.Marshal(params)
	return rows, out
}

// callerFields are the auth-record fields the generated rules read: the
// role and the data-subject id ("id" when the import reads the record id).
func (v *verifier) callerFields() (role, subject string) {
	role, subject = v.roleField, v.subjectField
	if role == "" {
		role = "role"
	}
	if subject == "" {
		subject = "id"
	}
	return role, subject
}
