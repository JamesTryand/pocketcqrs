package emschema

import (
	"sort"
	"strings"
)

// The personal-data rules the schema leaves to a generator (3.0.0: "not
// enforced here"), the same ones dotnetcqrs's DocumentMapper enforces, so a
// document imports on both stacks or neither. All fail closed: a pii field
// this project can't encrypt correctly is an import error, never a guess.

// piiLookalikeNames are subject-field names that suggest the id IS personal
// data. Erasure is terminal and ids are never reused (a returning person is
// a new subject), so an email-as-subject would either hit the terminal
// state on a rejoin or silently re-link the person erasure disconnected.
var piiLookalikeNames = []string{"email", "mail", "phone", "mobile", "telephone", "msisdn", "ssn", "nino", "passport"}

// checkPII applies every personal-data rule to doc.
func checkPII(doc *Document, r *Report) {
	for _, id := range sortedKeys(doc.Commands) {
		checkPIISubjects(r, "command "+id, doc.Commands[id].Fields)
	}
	for _, id := range sortedKeys(doc.Events) {
		checkPIISubjects(r, "event "+id, doc.Events[id].Fields)
	}
	for _, id := range sortedKeys(doc.ReadModels) {
		rm := doc.ReadModels[id]
		checkPIISubjects(r, "read model "+id, rm.Fields)
		checkReadModelPII(doc, r, id, rm)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkPIISubjects: every pii field names a sibling piiSubject that exists
// and is not itself pii; piiSubject only on a pii field; no pii inside
// subfields (not supported here yet); no derived pii field.
func checkPIISubjects(r *Report, owner string, fields []Field) {
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	for _, f := range fields {
		where := owner + "." + f.Name
		if hasNestedPII(f.Subfields) {
			r.errorf("%s has a pii subfield; personal data inside subfields is not supported here yet — make it a top-level field", where)
		}
		if f.PIISubject != "" && !f.PII {
			r.errorf("%s declares piiSubject %q but is not pii", where, f.PIISubject)
			continue
		}
		if !f.PII {
			continue
		}
		if f.Derivation != nil {
			r.errorf("%s is derived and pii; a derived value is computed in the clear, so it can't be personal data", where)
			continue
		}
		if f.PIISubject == "" {
			r.errorf("%s is pii but declares no piiSubject (the sibling field holding the data subject's id); "+
				"there is no safe default — the stream id is wrong whenever a stream holds someone else's data", where)
			continue
		}
		subject, ok := byName[f.PIISubject]
		if !ok {
			r.errorf("%s names piiSubject %q, which is not a sibling field", where, f.PIISubject)
			continue
		}
		if subject.PII {
			r.errorf("%s names piiSubject %q, which is itself pii and so cannot be encrypted under its own key", where, f.PIISubject)
			continue
		}
		if FoldType(subject) != "text" {
			r.errorf("%s names piiSubject %q, which is not a text field; a subject id is an opaque string", where, f.PIISubject)
			continue
		}
		lower := strings.ToLower(f.PIISubject)
		for _, w := range piiLookalikeNames {
			if strings.Contains(lower, w) {
				r.warnf("%s uses %q as its piiSubject, which looks like personal data; erasure is terminal and a "+
					"returning person is a new subject, so prefer an opaque id", where, f.PIISubject)
				break
			}
		}
	}
}

func hasNestedPII(fields []Field) bool {
	for _, f := range fields {
		if f.PII || hasNestedPII(f.Subfields) {
			return true
		}
	}
	return false
}

// checkReadModelPII: a read-model column agrees with the pii flag of every
// same-named field on its contributing events (both directions — inferring
// either way would store plaintext as ciphertext's column or vice versa);
// nothing compared in SQL (scopes, dateRange filters, derivation keys and
// amounts) is pii, because an encrypted value can't be compared.
func checkReadModelPII(doc *Document, r *Report, id string, rm ReadModel) {
	own := map[string]Field{}
	for _, f := range rm.Fields {
		own[f.Name] = f
	}
	for _, f := range rm.Fields {
		if f.IDAttribute && f.PII {
			r.errorf("read model %q: key field %q is pii; a row key is compared and looked up, so it can't be encrypted", id, f.Name)
		}
		if f.Derivation != nil {
			checkDerivationPII(doc, r, id, f)
			continue
		}
		for _, evID := range rm.BuiltFromEventIDs {
			ev, ok := doc.Events[evID]
			if !ok {
				continue
			}
			for _, ef := range ev.Fields {
				if ef.Name != f.Name || ef.PII == f.PII {
					continue
				}
				if f.PII {
					r.errorf("read model %q: field %q is pii but event %q carries it as plaintext; mark both or neither", id, f.Name, evID)
				} else {
					r.errorf("read model %q: field %q is not pii but event %q carries it as pii; the column would hold "+
						"ciphertext it can't use — mark the read-model field pii too", id, f.Name, evID)
				}
			}
		}
	}
	for _, sc := range rm.Scopes {
		if f, ok := own[sc.Via.FilterLocalField]; ok && f.PII {
			r.errorf("read model %q: scope %q filters on pii field %q; an encrypted column cannot be compared in SQL", id, sc.Param, f.Name)
		}
		if via, ok := doc.ReadModels[sc.Via.ReadModelID]; ok {
			for _, vf := range via.Fields {
				if vf.PII && (vf.Name == sc.Via.MatchParamTo || vf.Name == sc.Via.SelectField) {
					r.errorf("read model %q: scope %q joins on pii field %q of read model %q; an encrypted column cannot be compared in SQL",
						id, sc.Param, vf.Name, sc.Via.ReadModelID)
				}
			}
		}
	}
	subjects := map[string]bool{}
	for _, f := range rm.Fields {
		if f.PII {
			subjects[f.PIISubject] = true
		}
	}
	if len(subjects) > 0 && len(rm.RequiredRole) == 0 {
		r.warnf("read model %q holds personal data but declares no requiredRole; its read route serves it to "+
			"nobody but superusers (and, with the self-access hook, each subject for their own rows) — declare a role", id)
	}
	if len(subjects) > 1 {
		r.warnf("read model %q holds personal data of more than one subject per row; self-access (a person "+
			"reading their own rows) is unavailable for it, since a row would also carry someone else's data", id)
	}
	for _, filt := range rm.Filters {
		if f, ok := own[filt.Field]; ok && f.PII && filt.Kind == FilterDateRange {
			r.errorf("read model %q: dateRange filter %q is over pii field %q; an encrypted column cannot be compared in SQL", id, filt.Param, f.Name)
		}
	}
}

// checkDerivationPII: the payload fields a derivation reads to find its row
// or its amount must be plaintext on every event that feeds it.
func checkDerivationPII(doc *Document, r *Report, id string, f Field) {
	d := f.Derivation
	keys := map[string]string{"rowKeyField": d.RowKeyField, "amountField": d.AmountField, "groupByField": d.GroupByField}
	evIDs := derivationEvents(d)
	for _, sub := range f.Subfields {
		if sub.Derivation != nil {
			evIDs = append(evIDs, derivationEvents(sub.Derivation)...)
		}
	}
	for _, role := range []string{"rowKeyField", "amountField", "groupByField"} {
		name := keys[role]
		if name == "" {
			continue
		}
		for _, evID := range evIDs {
			for _, ef := range doc.Events[evID].Fields {
				if ef.Name == name && ef.PII {
					r.errorf("read model %q: derived field %q uses %s %q, which event %q carries as pii; "+
						"a derivation can't key or sum on an encrypted value", id, f.Name, role, name, evID)
				}
			}
		}
	}
}

// derivationEvents is every event id a derivation listens to.
func derivationEvents(d *FieldDerivation) []string {
	var out []string
	for _, ids := range [][]string{d.OnEventIDs, d.OffEventIDs, d.IncrementOnEventIDs,
		d.DecrementOnEventIDs, d.AddOnEventIDs, d.SubtractOnEventIDs} {
		out = append(out, ids...)
	}
	return out
}
