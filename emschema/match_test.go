package emschema

import (
	"strings"
	"testing"
)

// matchDocument is dateRangeDocument with one match filter (schema 3.1.0)
// added over staffId. A match filter is checked but not generated yet, so a
// well-formed one imports and is reported lossy, by name.
func matchDocument(f ReadModelFilter) *Document {
	doc := dateRangeDocument()
	rm := doc.ReadModels["time-entries"]
	rm.Filters = append(rm.Filters, f)
	doc.ReadModels["time-entries"] = rm
	return doc
}

func intp(n int) *int { return &n }

func TestMatchFilterImportsAndIsReportedLossy(t *testing.T) {
	mapped, err := Map(matchDocument(ReadModelFilter{
		Param: "staff", Field: "staffId", Kind: FilterMatch, Mode: MatchModeExact,
	}), Options{})
	if err != nil {
		t.Fatalf("a well-formed match filter must import: %v", err)
	}
	found := false
	for _, l := range mapped.Report.Lossy {
		if strings.Contains(l, `"staff"`) && strings.Contains(l, "not generated") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the dropped match filter must be named as lossy: %v", mapped.Report.Lossy)
	}
	for _, d := range mapped.Domains {
		for _, rm := range d.ReadModels {
			for _, f := range rm.Filters {
				if f.Kind == FilterMatch {
					t.Fatalf("a match filter must not reach scaffold until search is generated: %+v", f)
				}
			}
		}
	}
}

// TestMatchFilterRulesRejected: the reference and shape rules the schema
// leaves to a generator, the same ones dotnetcqrs's mapper enforces.
func TestMatchFilterRulesRejected(t *testing.T) {
	cases := []struct {
		name   string
		filter ReadModelFilter
		pii    bool
		want   string
	}{
		{"unknown field", ReadModelFilter{Param: "q", Field: "nope", Kind: FilterMatch, Mode: MatchModeExact}, false, "nope"},
		{"bad mode", ReadModelFilter{Param: "q", Field: "staffId", Kind: FilterMatch, Mode: "phonetic"}, false, "phonetic"},
		{"bad normalize", ReadModelFilter{Param: "q", Field: "staffId", Kind: FilterMatch, Mode: MatchModeExact, Normalize: "soundex"}, false, "soundex"},
		{"minPrefixLength off prefix", ReadModelFilter{Param: "q", Field: "staffId", Kind: FilterMatch, Mode: MatchModeExact, MinPrefixLength: intp(3)}, false, "minPrefixLength"},
		{"not text", ReadModelFilter{Param: "q", Field: "taskDate", Kind: FilterMatch, Mode: MatchModeExact}, false, "not a text field"},
		{"pii prefix without minPrefixLength", ReadModelFilter{Param: "q", Field: "staffId", Kind: FilterMatch, Mode: MatchModePrefix}, true, "minPrefixLength"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := matchDocument(c.filter)
			if c.pii {
				rm := doc.ReadModels["time-entries"]
				rm.Fields[1].PII = true
				rm.Fields[1].PIISubject = "entryId"
				doc.ReadModels["time-entries"] = rm
				ev := doc.Events["time-logged"]
				ev.Fields[1].PII = true
				ev.Fields[1].PIISubject = "entryId"
				doc.Events["time-logged"] = ev
			}
			_, err := Map(doc, Options{})
			if err == nil {
				t.Fatal("expected the match filter to be refused")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("the refusal must mention %q: %v", c.want, err)
			}
		})
	}
}
