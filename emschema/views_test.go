package emschema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secondView writes another coordinating document beside the first, naming
// the same two documents under its own keys.
func secondView(t *testing.T, dir, id, edge string) string {
	t.Helper()
	p := filepath.Join(dir, id+".architecture.json")
	body := `{
  "architectureSchemaVersion": "0.3.0", "id": "` + id + `", "name": "Another view",
  "systems": {
    "readers": { "name": "Members", "document": { "path": "members.eventmodel.json" } },
    "lending": { "name": "Lending", "document": { "path": "lending.eventmodel.json" } }
  },
  "edges": [` + edge + `]
}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func edgesInViews(t *testing.T, paths []string, systems []string, docPath string) (*SystemEdges, error) {
	t.Helper()
	var views []*Architecture
	for _, p := range paths {
		a, err := LoadArchitecture(p)
		if err != nil {
			t.Fatal(err)
		}
		views = append(views, a)
	}
	doc, err := Load(docPath)
	if err != nil {
		t.Fatal(err)
	}
	return EdgesForViews(views, systems, doc, docPath, Options{})
}

// The same link held in two views, each naming the systems its own way: the
// receiver is found in each by its document, and gets the edge once.
func TestALinkInTwoViewsIsReceivedOnce(t *testing.T) {
	arch, dir := viewOf(t, membersDoc, byPath, peopleToLoans)
	same := strings.NewReplacer(`"people"`, `"readers"`, `"loans"`, `"lending"`).Replace(peopleToLoans)
	other := secondView(t, dir, "branches", same)
	lending := filepath.Join(dir, "lending.eventmodel.json")
	se, err := edgesInViews(t, []string{arch, other}, nil, lending)
	if err != nil {
		t.Fatal(err)
	}
	if len(se.In) != 1 || se.In[0].ID != "member-becomes-borrower" || se.In[0].FromSystem != "members" {
		t.Fatalf("want the one edge from members, got %+v", se.In)
	}
	if se.Architecture != "library, branches" {
		t.Fatalf("want both views named, got %q", se.Architecture)
	}
}

// A link held in only one of the views is received: the other view's links
// leave it out, and they do not contradict.
func TestALinkInOneViewOfTwoIsReceived(t *testing.T) {
	arch, dir := viewOf(t, membersDoc, byPath, peopleToLoans)
	other := secondView(t, dir, "branches", "")
	se, err := edgesInViews(t, []string{other, arch}, nil, filepath.Join(dir, "lending.eventmodel.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(se.In) != 1 {
		t.Fatalf("want the edge from the view that holds it, got %+v", se.In)
	}
}

// Two views giving one edge id different translations contradict: refused,
// naming the edge and both views.
func TestViewsThatContradictAreRefused(t *testing.T) {
	arch, dir := viewOf(t, membersDoc, byPath, peopleToLoans)
	changed := strings.NewReplacer(`"people"`, `"readers"`, `"loans"`, `"lending"`,
		`, "branchId": "branchId" }`, ` }`, `"fields": { "borrowerId": "m-1", "branchId": "north" }`,
		`"fields": { "borrowerId": "m-1" }`).Replace(peopleToLoans)
	other := secondView(t, dir, "branches", changed)
	_, err := edgesInViews(t, []string{arch, other}, nil, filepath.Join(dir, "lending.eventmodel.json"))
	if err == nil || !strings.Contains(err.Error(), `edge "member-becomes-borrower" is one link in "library" and another in "branches"`) {
		t.Fatalf("want the contradiction refused, got %v", err)
	}
}

// A view that does not name this document cannot say which system it is.
func TestAViewNotNamingTheDocumentIsRefused(t *testing.T) {
	arch, dir := viewOf(t, membersDoc, byPath, peopleToLoans)
	stray := filepath.Join(dir, "stray.architecture.json")
	if err := os.WriteFile(stray, []byte(`{ "architectureSchemaVersion": "0.3.0", "id": "stray", "name": "Stray",
  "systems": { "elsewhere": { "name": "Elsewhere" } }, "edges": [] }`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := edgesInViews(t, []string{arch, stray}, nil, filepath.Join(dir, "lending.eventmodel.json"))
	if err == nil || !strings.Contains(err.Error(), `"stray" names no system that is this document`) {
		t.Fatalf("want the view refused, got %v", err)
	}
}

// The single-view form is unchanged: one name given is the system in it.
func TestOneViewWithItsSystemNamedIsAsBefore(t *testing.T) {
	arch, dir := viewOf(t, membersDoc, byPath, peopleToLoans)
	lending := filepath.Join(dir, "lending.eventmodel.json")
	one, err := edgesInViews(t, []string{arch}, []string{"loans"}, lending)
	if err != nil {
		t.Fatal(err)
	}
	before, err := edgesIn(t, arch, "loans", lending)
	if err != nil {
		t.Fatal(err)
	}
	if len(one.In) != len(before.In) || one.Architecture != before.Architecture || one.System != before.System {
		t.Fatalf("want the same as EdgesFor, got %+v and %+v", one, before)
	}
}
