// Package queryroute serves a read model over HTTP: one generated GET
// route per read model, at /api/cqrs/rm/{collection}. It is the read path
// for personal data — the port of dotnetcqrs's generated read-model query
// routes — and works the same for a read model without any.
//
// What it guarantees (user's decisions, 2026-10-06; see the notebook's
// platform/eventmodeling-codegen findings.md "G6 decisions"):
//
//   - Authorization is the read model's requiredRole, resolved by the same
//     kind of host hook the command side uses (ResolveOwnRole). Superusers
//     see every row.
//   - Self-access: a caller without the role may still read the rows whose
//     subject column holds their own subject id, when the host sets the
//     default-off SubjectOf hook — a person reading their own data (GDPR
//     Art. 15). Only for a read model whose pii columns share one subject
//     column, so a row can't also carry someone else's personal data.
//   - Personal data is revealed per page with one decrypt-batch per data
//     subject (package pii's Buffer and per-process Cache). An erased value
//     is {"$redacted":true}, distinct from null. A pii cell that isn't an
//     envelope fails closed.
//   - A pii column can't be filtered or sorted on: it is ciphertext, so the
//     comparison would be meaningless (400, not a silent empty result).
//   - Registering the route for a collection with pii columns makes it the
//     only read path: the collection's List and View rules are forced to
//     superuser-only, so PocketBase's own record API (list, view, realtime,
//     expand) refuses everyone else.
package queryroute

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/kms"
	"github.com/jamestryand/pocketcqrs/pii"
)

// PathPrefix is where every read-model route lives.
const PathPrefix = "/api/cqrs/rm/"

// Spec is one read model's route, generated from the document.
type Spec struct {
	Collection string
	Key        string
	// Columns are returned in every row (the key included).
	Columns []string
	// PII maps each personal-data column to the column holding its subject.
	PII map[string]string
	// RequiredRole: any one of these roles reads every row. Empty: any
	// authenticated caller does — unless the read model holds personal
	// data, where empty means nobody but superusers (and, with SubjectOf,
	// each subject for their own rows): personal data is never readable by
	// default.
	RequiredRole []string
	// SelfSubject is the one subject column every pii column shares, when
	// there is one; self-access is only possible then.
	SelfSubject string
}

// Config is the host's part.
type Config struct {
	// PII is required when the spec has pii columns (pii.Wire returns it).
	PII *pii.Deps
	// ResolveOwnRole returns the caller's role ("" for none), as the
	// command side's authorize.Authorizer does.
	ResolveOwnRole func(auth *core.Record) string
	// SubjectOf returns the caller's own data-subject id ("" for none). nil
	// (the default) means no self-access.
	SubjectOf func(auth *core.Record) string
	// Logger may be nil.
	Logger func(msg string, args ...any)
}

const (
	defaultPerPage = 50
	maxPerPage     = 500
)

// RedactedMarker is what an erased value reads as. The same shape
// dotnetcqrs returns, so a client sees one contract on both stacks.
var RedactedMarker = map[string]any{"$redacted": true}

func (s Spec) validate(cfg Config) error {
	cols := map[string]bool{}
	for _, c := range s.Columns {
		cols[c] = true
	}
	if !cols[s.Key] {
		return fmt.Errorf("queryroute %s: key %q is not a column", s.Collection, s.Key)
	}
	for col, subj := range s.PII {
		if !cols[col] || !cols[subj] {
			return fmt.Errorf("queryroute %s: pii column %q and its subject %q must both be columns", s.Collection, col, subj)
		}
		if s.PII[subj] != "" {
			return fmt.Errorf("queryroute %s: subject column %q is itself pii", s.Collection, subj)
		}
	}
	if s.SelfSubject != "" {
		for _, subj := range s.PII {
			if subj != s.SelfSubject {
				return fmt.Errorf("queryroute %s: self-access needs every pii column to share subject %q", s.Collection, s.SelfSubject)
			}
		}
	}
	if len(s.PII) > 0 && (cfg.PII == nil || cfg.PII.KMS == nil) {
		return fmt.Errorf("queryroute %s: has personal-data columns but no pii.Deps (call pii.Wire)", s.Collection)
	}
	if len(s.RequiredRole) > 0 && cfg.ResolveOwnRole == nil {
		return fmt.Errorf("queryroute %s: declares requiredRole %v but no ResolveOwnRole hook", s.Collection, s.RequiredRole)
	}
	return nil
}

// Register adds GET /api/cqrs/rm/{collection} and, for a collection with
// personal-data columns, locks PocketBase's own record API to superusers.
// It fails rather than serve a misconfigured route.
func Register(se *core.ServeEvent, spec Spec, cfg Config) error {
	if err := spec.validate(cfg); err != nil {
		return err
	}
	if cfg.Logger == nil {
		cfg.Logger = func(string, ...any) {}
	}
	if len(spec.PII) > 0 {
		if err := LockRecordAPI(se.App, spec.Collection, cfg.Logger); err != nil {
			return err
		}
	}
	se.Router.GET(PathPrefix+spec.Collection, func(re *core.RequestEvent) error {
		return serve(re, spec, cfg)
	}).Bind(apis.RequireAuth())
	return nil
}

// LockRecordAPI forces a collection's List and View rules to superuser-only
// (nil), so PocketBase's record API — list, view, realtime and expand from
// another collection — refuses everyone but superusers. Idempotent; logs
// when it changes a rule, because it overrides what a migration set.
func LockRecordAPI(app core.App, collection string, logger func(string, ...any)) error {
	col, err := app.FindCollectionByNameOrId(collection)
	if err != nil {
		return fmt.Errorf("queryroute: collection %q: %w", collection, err)
	}
	if col.ListRule == nil && col.ViewRule == nil {
		return nil
	}
	col.ListRule, col.ViewRule = nil, nil
	if err := app.Save(col); err != nil {
		return fmt.Errorf("queryroute: locking %q to superusers: %w", collection, err)
	}
	if logger != nil {
		logger("queryroute: collection holds personal data; its List/View rules were set to superuser-only "+
			"so the generated read route is the only way to read it", "collection", collection)
	}
	return nil
}

func fail(re *core.RequestEvent, status int, msg string) error {
	return re.JSON(status, map[string]string{"error": msg})
}

func serve(re *core.RequestEvent, spec Spec, cfg Config) error {
	// who may see what
	selfOnly := ""
	if !re.HasSuperuserAuth() && (len(spec.RequiredRole) > 0 || len(spec.PII) > 0) {
		role := ""
		if cfg.ResolveOwnRole != nil {
			role = cfg.ResolveOwnRole(re.Auth)
		}
		if !hasRole(spec.RequiredRole, role) {
			subject := ""
			if cfg.SubjectOf != nil && spec.SelfSubject != "" {
				subject = cfg.SubjectOf(re.Auth)
			}
			if subject == "" {
				return fail(re, http.StatusForbidden, "not authorized to read "+spec.Collection)
			}
			selfOnly = subject
		}
	}

	filter, params, sortExpr, page, perPage, err := parseQuery(re.Request.URL.Query(), spec)
	if err != nil {
		return fail(re, http.StatusBadRequest, err.Error())
	}
	if selfOnly != "" {
		filter = append(filter, spec.SelfSubject+" = {:self}")
		params["self"] = selfOnly
	}

	records, err := re.App.FindRecordsByFilter(spec.Collection, strings.Join(filter, " && "), sortExpr,
		perPage, (page-1)*perPage, params)
	if err != nil {
		return fail(re, http.StatusInternalServerError, "query failed: "+err.Error())
	}

	rows := make([]map[string]any, len(records))
	var buf *pii.Buffer
	if len(spec.PII) > 0 {
		buf = pii.NewBuffer(cfg.PII.KMS, cfg.PII.Cache)
	}
	for i, rec := range records {
		row := make(map[string]any, len(spec.Columns))
		for _, col := range spec.Columns {
			if _, isPII := spec.PII[col]; isPII {
				if err := queueReveal(buf, row, col, rec.GetString(col)); err != nil {
					cfg.Logger("queryroute: personal-data column does not hold an envelope", "collection", spec.Collection, "column", col, "error", err)
					return fail(re, http.StatusInternalServerError, fmt.Sprintf("%s.%s does not hold encrypted data; refusing to serve it", spec.Collection, col))
				}
				continue
			}
			row[col] = rec.Get(col)
		}
		rows[i] = row
	}
	if buf != nil {
		if err := buf.Flush(re.Request.Context()); err != nil {
			if kms.IsUnavailable(err) {
				re.Response.Header().Set("Retry-After", "1")
				return fail(re, http.StatusServiceUnavailable, "dependency unavailable: the key service is unavailable; retry later")
			}
			return fail(re, http.StatusInternalServerError, "revealing personal data failed: "+err.Error())
		}
	}
	return re.JSON(http.StatusOK, map[string]any{"items": rows, "page": page, "perPage": perPage})
}

// queueReveal reads one pii cell: empty is null, an erased envelope is the
// redacted marker, anything else is queued for the page's single flush.
// Plaintext in a pii column is an error: fail closed, never serve it.
func queueReveal(buf *pii.Buffer, row map[string]any, col, cell string) error {
	if strings.TrimSpace(cell) == "" || cell == "null" {
		row[col] = nil
		return nil
	}
	if !pii.IsEnvelope([]byte(cell)) {
		return errors.New("not a $pii envelope")
	}
	subject, cipher, hasCipher, err := pii.ParseEnvelope([]byte(cell))
	if err != nil {
		return err
	}
	if !hasCipher {
		row[col] = RedactedMarker
		return nil
	}
	buf.RevealRaw(subject, cipher, func(plain []byte, redacted bool) {
		if redacted {
			row[col] = RedactedMarker
			return
		}
		var v any
		if err := json.Unmarshal(plain, &v); err != nil {
			row[col] = string(plain)
			return
		}
		row[col] = v
	})
	return nil
}

func hasRole(roles []string, role string) bool {
	if role == "" {
		return false
	}
	for _, r := range roles {
		if strings.EqualFold(r, role) {
			return true
		}
	}
	return false
}

// parseQuery turns query params into a PocketBase filter over plain
// columns. Column names only ever come from the spec, values only ever
// travel as bound params. A pii column can't be filtered or sorted on; an
// unknown param is an error rather than silently ignored.
func parseQuery(q map[string][]string, spec Spec) (filter []string, params dbx.Params, sortExpr string, page, perPage int, err error) {
	params = dbx.Params{}
	page, perPage = 1, defaultPerPage
	plain := map[string]bool{}
	for _, c := range spec.Columns {
		if _, isPII := spec.PII[c]; !isPII {
			plain[c] = true
		}
	}
	names := make([]string, 0, len(q))
	for k := range q {
		names = append(names, k)
	}
	sort.Strings(names)
	for i, name := range names {
		val := q[name][0]
		switch {
		case name == "page" || name == "perPage":
			n, convErr := strconv.Atoi(val)
			if convErr != nil || n < 1 {
				return nil, nil, "", 0, 0, fmt.Errorf("%s must be a positive integer", name)
			}
			if name == "page" {
				page = n
			} else {
				perPage = min(n, maxPerPage)
			}
		case name == "sort":
			col := strings.TrimLeft(val, "-+")
			if _, isPII := spec.PII[col]; isPII {
				return nil, nil, "", 0, 0, fmt.Errorf("'%s' is personal data and is stored encrypted, so it cannot be sorted on", col)
			}
			if !plain[col] {
				return nil, nil, "", 0, 0, fmt.Errorf("cannot sort on unknown column '%s'", col)
			}
			sortExpr = val
		case spec.PII[name] != "":
			return nil, nil, "", 0, 0, fmt.Errorf("'%s' is personal data and is stored encrypted, so it cannot be used as a query filter", name)
		case plain[name]:
			p := "p" + strconv.Itoa(i)
			filter = append(filter, name+" = {:"+p+"}")
			params[p] = val
		default:
			return nil, nil, "", 0, 0, fmt.Errorf("unknown query parameter '%s'", name)
		}
	}
	return filter, params, sortExpr, page, perPage, nil
}
