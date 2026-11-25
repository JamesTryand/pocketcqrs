package emschema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/edges"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/scaffold"
)

// viewOf writes the members and lending documents and a coordinating document
// naming them as systems says (its "systems" object), and returns the view's
// path and the directory.
func viewOf(t *testing.T, members, systems, edge string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("members.eventmodel.json", members)
	write("lending.eventmodel.json", receivedDoc(`, "public": { "version": 1 }`))
	arch := write("library.architecture.json", `{
  "architectureSchemaVersion": "0.3.0", "id": "library", "name": "Library",
  "systems": `+systems+`,
  "edges": [`+edge+`]
}`)
	return arch, dir
}

func edgesIn(t *testing.T, arch, key, docPath string) (*SystemEdges, error) {
	t.Helper()
	a, err := LoadArchitecture(arch)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Load(docPath)
	if err != nil {
		t.Fatal(err)
	}
	return EdgesFor(a, key, doc, docPath, Options{})
}

// A view names its systems as it likes; the edge between them uses its names.
const peopleToLoans = `{ "id": "member-becomes-borrower", "kind": "internal",
  "from": { "system": "people", "event": "member-joined", "version": 1 },
  "to": { "system": "loans", "command": "register-borrower" },
  "targetId": "memberId", "fields": { "borrowerId": "memberId", "branchId": "branchId" },
  "examples": [{ "name": "a member becomes a borrower",
    "given": { "memberId": "m-1", "branchId": "north", "email": "reader@example.org" },
    "then": { "targetId": "m-1", "fields": { "borrowerId": "m-1", "branchId": "north" } } }] }`

const byPath = `{
    "people": { "name": "Members", "document": { "path": "members.eventmodel.json" } },
    "loans": { "name": "Lending", "document": { "path": "lending.eventmodel.json" } }
  }`

// A message names the system that sent it by that system's document id, not
// by the name a view gives it: the sender knows its id from its own document,
// and every view naming it agrees on that.
func TestAMessageNamesItsSenderByItsDocumentID(t *testing.T) {
	arch, dir := viewOf(t, membersDoc, byPath, peopleToLoans)
	out, err := edgesIn(t, arch, "people", filepath.Join(dir, "members.eventmodel.json"))
	if err != nil {
		t.Fatal(err)
	}
	if out.System != "members" || len(out.Out) != 1 || out.Out[0].System != "members" {
		t.Fatalf("want the sender named members, got %q, out %+v", out.System, out.Out)
	}
	src, err := scaffold.GenerateGoEdges("domain", out.Architecture, out.System, out.In, out.Out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(src, `const EdgesSystem = "members"`) {
		t.Errorf("edges.go does not name the sender by its document id:\n%s", src)
	}

	in, err := edgesIn(t, arch, "loans", filepath.Join(dir, "lending.eventmodel.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(in.In) != 1 || in.In[0].FromSystem != "members" {
		t.Fatalf("want the edge in from members, got %+v", in.In)
	}
	if len(in.Examples) != 1 || !in.Examples[0].Passed {
		t.Fatalf("the example did not hold: %+v", in.Examples)
	}
	// what the sender sends is what the receiver finds
	m, ok, err := edges.Publish(out.Out, events.Event{ID: "e-1", Type: "MemberJoined",
		Data: []byte(`{"memberId": "m-1", "branchId": "north", "email": "reader@example.org"}`)})
	if err != nil || !ok {
		t.Fatalf("the sender publishes nothing for MemberJoined: %v", err)
	}
	if len(edges.Find(in.In, m)) != 1 {
		t.Fatalf("the receiver finds no edge for %+v", m)
	}
}

// A placeholder has no document yet, so no id its messages would carry.
func TestAnEdgeFromAPlaceholderIsRefused(t *testing.T) {
	systems := `{
    "people": { "name": "Members", "placeholder": { "publishes": [{ "event": "member-joined", "version": 1,
      "fields": [{ "name": "memberId", "type": "string" }, { "name": "branchId", "type": "string" }] }] } },
    "loans": { "name": "Lending", "document": { "path": "lending.eventmodel.json" } }
  }`
	arch, dir := viewOf(t, membersDoc, systems, peopleToLoans)
	_, err := edgesIn(t, arch, "loans", filepath.Join(dir, "lending.eventmodel.json"))
	if err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("want the edge from a placeholder refused, got %v", err)
	}
}

// A system named by release is found by its document id: the receiver's own
// document is the one the release names, and a sender named so is named by it.
func TestSystemsNamedByReleaseAreNamedByTheirDocumentID(t *testing.T) {
	systems := `{
    "people": { "name": "Members", "release": { "system": "members", "tag": "v1.0.0" } },
    "loans": { "name": "Lending", "release": { "system": "lending", "tag": "v2.0.0" } }
  }`
	arch, dir := viewOf(t, membersWithFields(`"memberId", "branchId"`), systems, peopleToLoans)
	in, err := edgesIn(t, arch, "loans", filepath.Join(dir, "lending.eventmodel.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(in.In) != 1 || in.In[0].FromSystem != "members" || !in.Examples[0].Passed {
		t.Fatalf("want the edge in from members, got %+v, %+v", in.In, in.Examples)
	}
	out, err := edgesIn(t, arch, "people", filepath.Join(dir, "members.eventmodel.json"))
	if err != nil {
		t.Fatal(err)
	}
	if out.System != "members" || len(out.Out) != 1 {
		t.Fatalf("got %q, out %+v", out.System, out.Out)
	}
}

func TestADocumentThatIsNotTheReleasedSystemIsRefused(t *testing.T) {
	systems := `{
    "people": { "name": "Members", "release": { "system": "members", "tag": "v1.0.0" } },
    "loans": { "name": "Lending", "release": { "system": "lending", "tag": "v2.0.0" } }
  }`
	arch, dir := viewOf(t, membersWithFields(`"memberId", "branchId"`), systems, peopleToLoans)
	_, err := edgesIn(t, arch, "people", filepath.Join(dir, "lending.eventmodel.json"))
	if err == nil || !strings.Contains(err.Error(), `"members"`) {
		t.Fatalf("want the wrong document refused, got %v", err)
	}
}

// A sender's build needs only its own document: each public event whose
// marker lists its fields is published with those, under the document's id.
func TestASenderPublishesFromItsOwnDocumentAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "members.eventmodel.json")
	if err := os.WriteFile(path, []byte(membersWithFields(`"memberId", "branchId"`)), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	se, err := PublishedEdges(doc)
	if err != nil {
		t.Fatal(err)
	}
	if se == nil || se.System != "members" || len(se.In) != 0 || len(se.Out) != 1 ||
		se.Out[0].System != "members" || strings.Join(se.Out[0].Fields, ",") != "branchId,memberId" {
		t.Fatalf("got %+v", se)
	}
}

// Without a list an event's published fields are those some edge maps, and
// with no connecting document there is no edge: nothing is published.
func TestAnEventWithoutAListIsNotPublishedAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "members.eventmodel.json")
	if err := os.WriteFile(path, []byte(membersDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	se, err := PublishedEdges(doc)
	if err != nil || se != nil {
		t.Fatalf("want nothing published, got %+v, %v", se, err)
	}
}

func TestAListedPersonalFieldIsRefusedAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "members.eventmodel.json")
	if err := os.WriteFile(path, []byte(membersWithFields(`"memberId", "email"`)), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PublishedEdges(doc); err == nil || !strings.Contains(err.Error(), `"email" is personal data`) {
		t.Fatalf("want the personal field refused, got %v", err)
	}
}
