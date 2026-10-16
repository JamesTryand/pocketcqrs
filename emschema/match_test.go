package emschema

import (
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/scaffold"
)

// matchDocument is dateRangeDocument with one match filter (schema 3.1.0)
// added over agentId. A match filter is checked but not generated yet, so a
// well-formed one imports and is reported lossy, by name.
func matchDocument(f ReadModelFilter) *Document {
	doc := dateRangeDocument()
	rm := doc.ReadModels["tickets"]
	rm.Filters = append(rm.Filters, f)
	doc.ReadModels["tickets"] = rm
	return doc
}

func intp(n int) *int { return &n }

func TestMatchFilterIsCarriedToScaffold(t *testing.T) {
	mapped, err := Map(matchDocument(ReadModelFilter{
		Param: "agent", Field: "agentId", Kind: FilterMatch, Mode: MatchModePrefix,
	}), Options{})
	if err != nil {
		t.Fatalf("a well-formed match filter must import: %v", err)
	}
	for _, l := range mapped.Report.Lossy {
		if strings.Contains(l, `"agent"`) {
			t.Fatalf("a match filter is generated now, not lossy: %s", l)
		}
	}
	var got []scaffold.ReadModelFilter
	for _, d := range mapped.Domains {
		for _, rm := range d.ReadModels {
			for _, f := range rm.Filters {
				if f.Kind == FilterMatch {
					got = append(got, f)
				}
			}
		}
	}
	if len(got) != 1 || got[0].Mode != MatchModePrefix || got[0].MinPrefixLength != 3 {
		t.Fatalf("carried with the schema's default minPrefixLength of 3: %+v", got)
	}
}

func piiMatchDocument(filters ...ReadModelFilter) *Document {
	doc := matchDocument(filters[0])
	rm := doc.ReadModels["tickets"]
	rm.Filters = append(rm.Filters, filters[1:]...)
	rm.Fields[1].PII, rm.Fields[1].PIISubject = true, "ticketId"
	doc.ReadModels["tickets"] = rm
	ev := doc.Events["ticket-opened"]
	ev.Fields[1].PII, ev.Fields[1].PIISubject = true, "ticketId"
	doc.Events["ticket-opened"] = ev
	return doc
}

func TestPIIMatchWarningsAndTheSharedPrefixRule(t *testing.T) {
	mapped, err := Map(piiMatchDocument(
		ReadModelFilter{Param: "s", Field: "agentId", Kind: FilterMatch, Mode: MatchModePrefix, MinPrefixLength: intp(3)},
		ReadModelFilter{Param: "c", Field: "agentId", Kind: FilterMatch, Mode: MatchModeContains},
	), Options{})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(mapped.Report.Warnings, " | ")
	if !strings.Contains(all, "hmac endpoint") || !strings.Contains(all, "PLAINTEXT index") {
		t.Fatalf("pii prefix and pii contains must each say what they cost: %v", mapped.Report.Warnings)
	}
	_, err = Map(piiMatchDocument(
		ReadModelFilter{Param: "a", Field: "agentId", Kind: FilterMatch, Mode: MatchModePrefix, MinPrefixLength: intp(3)},
		ReadModelFilter{Param: "b", Field: "agentId", Kind: FilterMatch, Mode: MatchModePrefix, MinPrefixLength: intp(4)},
	), Options{})
	if err == nil || !strings.Contains(err.Error(), "share one hashed prefix index") {
		t.Fatalf("two hashed prefix filters on one index must agree on minPrefixLength: %v", err)
	}
}

func TestVerifyAppliesAMatchFilter(t *testing.T) {
	rows := map[string]map[string]any{
		"t1": {"agentId": "Alice"}, "t2": {"agentId": "alan"}, "t3": {"agentId": "Bob"},
	}
	for mode, want := range map[string]int{MatchModeExact: 0, MatchModePrefix: 2, MatchModeContains: 2} {
		got, err := applyMatch(ReadModelFilter{Field: "agentId", Mode: mode}, rows, "AL")
		if err != nil || len(got) != want {
			t.Errorf("%s: want %d rows, got %d %v", mode, want, len(got), err)
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
		{"bad mode", ReadModelFilter{Param: "q", Field: "agentId", Kind: FilterMatch, Mode: "phonetic"}, false, "phonetic"},
		{"bad normalize", ReadModelFilter{Param: "q", Field: "agentId", Kind: FilterMatch, Mode: MatchModeExact, Normalize: "soundex"}, false, "soundex"},
		{"minPrefixLength off prefix", ReadModelFilter{Param: "q", Field: "agentId", Kind: FilterMatch, Mode: MatchModeExact, MinPrefixLength: intp(3)}, false, "minPrefixLength"},
		{"not text", ReadModelFilter{Param: "q", Field: "openedOn", Kind: FilterMatch, Mode: MatchModeExact}, false, "not a text field"},
		{"pii prefix without minPrefixLength", ReadModelFilter{Param: "q", Field: "agentId", Kind: FilterMatch, Mode: MatchModePrefix}, true, "minPrefixLength"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := matchDocument(c.filter)
			if c.pii {
				rm := doc.ReadModels["tickets"]
				rm.Fields[1].PII = true
				rm.Fields[1].PIISubject = "ticketId"
				doc.ReadModels["tickets"] = rm
				ev := doc.Events["ticket-opened"]
				ev.Fields[1].PII = true
				ev.Fields[1].PIISubject = "ticketId"
				doc.Events["ticket-opened"] = ev
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
