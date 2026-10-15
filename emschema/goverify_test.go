package emschema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goVerify runs doc's scenarios against Go code, built against this
// checkout.
func goVerify(t *testing.T, doc *Document, mapped *Mapped, host string) map[string]ScenarioResult {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	results, err := VerifyWith(doc, mapped, t.TempDir(), VerifyOptions{Lang: "go", Pocketcqrs: root, Host: host})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]ScenarioResult{}
	for _, r := range results {
		out[r.ScenarioID] = r
	}
	return out
}

func loadMapped(t *testing.T, path string) (*Document, *Mapped) {
	t.Helper()
	doc, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return doc, mapped
}

// The generated Go answers as the generated JS does: the declared outcomes
// pass, the refusal is its own, and the hand-written condition is HAND (its
// Go stand-in panics the marker).
func TestGoVerifyGenerated(t *testing.T) {
	doc, mapped := loadMapped(t, fixture(t, "examples/conditional-outcomes.json"))
	results := goVerify(t, doc, mapped, "")
	for id, r := range results {
		want := id != "expense-over-limit"
		if r.Passed != want || r.Hand == want {
			t.Errorf("%s: passed=%v hand=%v: %s", id, r.Passed, r.Hand, r.Detail)
		}
	}
	if len(results) != 4 {
		t.Fatalf("want 4 scenarios, got %d", len(results))
	}
}

// A host's own Go package runs: its hook decides the over-limit expense
// (a pass, not HAND), and a package the host lacks is a skip.
func TestGoVerifyHost(t *testing.T) {
	doc, mapped := loadMapped(t, fixture(t, "examples/conditional-outcomes.json"))
	host := t.TempDir()
	for _, d := range mapped.Domains {
		if d.Aggregate != "expense" {
			continue // the host has no entry package
		}
		files, err := d.GenerateGo()
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(host, "expense")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name, ".example") {
				if err := os.WriteFile(filepath.Join(dir, f.Name), []byte(f.Source), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		hand := "package expense\n\nimport (\n\t\"encoding/json\"\n\n\t\"github.com/jamestryand/pocketcqrs/decider\"\n)\n\n" +
			"func SubmitExpenseOutcome0(s State, cmd decider.Command) bool {\n" +
			"\tvar p struct{ Amount float64 `json:\"amount\"` }\n\t_ = json.Unmarshal(cmd.Payload, &p)\n\treturn p.Amount > 500\n}\n"
		if err := os.WriteFile(filepath.Join(dir, "expense_hand.go"), []byte(hand), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	results := goVerify(t, doc, mapped, host)
	if r := results["expense-over-limit"]; !r.Passed || r.Hand {
		t.Errorf("the host's hook: passed=%v hand=%v: %s", r.Passed, r.Hand, r.Detail)
	}
	for id, r := range results {
		if id != "expense-over-limit" && (!r.Skipped || !strings.Contains(r.Detail, "the host has no Go decider")) {
			t.Errorf("%s: skipped=%v: %s", id, r.Skipped, r.Detail)
		}
	}
}

// View scenarios run the Go projections against PocketBase: a toggle's
// events merge their fields, and the rows read back as PocketBase serves
// them.
func TestGoVerifyViews(t *testing.T) {
	doc, mapped := loadMapped(t, writeDoc(t, toggleSeedDoc))
	for id, r := range goVerify(t, doc, mapped, "") {
		if strings.HasPrefix(id, "open") || strings.HasPrefix(id, "closed") {
			if !r.Passed {
				t.Errorf("%s: %s", id, r.Detail)
			}
		}
	}
}

// Upstream's rows-per-list-item.json through the Go projection: the
// parts and the discard pass; a draft's subtotal, null in the model, reads
// 0 from a PocketBase number field, and the Go check says so where the JS
// dry run can't.
func TestGoVerifyRowsPerListItem(t *testing.T) {
	doc, mapped := loadMapped(t, fixture(t, "examples/rows-per-list-item.json"))
	results := goVerify(t, doc, mapped, "")
	for id, r := range results {
		wantPass := id != "a-drafts-parts-have-no-subtotal"
		if r.Passed != wantPass {
			t.Errorf("%s: passed=%v: %s", id, r.Passed, r.Detail)
		}
	}
	if r := results["a-drafts-parts-have-no-subtotal"]; !strings.Contains(r.Detail, "subtotal") {
		t.Errorf("the draft's failure isn't about its subtotal: %s", r.Detail)
	}
}

// A personal-data column through a Go projection holds the envelope's JSON
// text; the harness reads it back revealed, so the model's plain value
// matches.
const piiViewDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"p","name":"P",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{"noted":{"name":"Noted","swimlaneId":"s","aggregate":"note","fields":[{"name":"noteId","type":"string","idAttribute":true},{"name":"staffId","type":"string"},{"name":"text","type":"string","pii":true,"piiSubject":"staffId"}]}},
	"commands":{"take":{"name":"Take","aggregate":"note","fields":[{"name":"noteId","type":"string","idAttribute":true},{"name":"staffId","type":"string"},{"name":"text","type":"string","pii":true,"piiSubject":"staffId"}]}},
	"readModels":{"notes":{"name":"Notes","builtFromEventIds":["noted"],
		"fields":[{"name":"noteId","type":"string","idAttribute":true},{"name":"staffId","type":"string"},{"name":"text","type":"string","pii":true,"piiSubject":"staffId"}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"t","name":"T","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"take","eventIds":["noted"],"scenarios":[]},
		{"id":"v","name":"V","pattern":"stateView","swimlaneId":"s","status":"planned","screenId":"sc","readModelId":"notes","scenarios":[
			{"id":"plain","name":"plain","kind":"stateView","given":[{"eventId":"noted","data":{"noteId":"n1","staffId":"s1","text":"call Ada"}}],
			 "when":{"readModelId":"notes"},"then":{"result":{"entries":[{"noteId":"n1","text":"call Ada"}]}}}]}]}`

func TestGoVerifyRevealsPersonalData(t *testing.T) {
	doc, mapped := loadMapped(t, writeDoc(t, piiViewDoc))
	if r := goVerify(t, doc, mapped, "")["plain"]; !r.Passed {
		t.Fatalf("%s", r.Detail)
	}
}
