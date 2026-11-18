package emschema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/scaffold"
)

const membersDoc = `{
  "eventModelingSchemaVersion": "6.1.0", "id": "members", "name": "Members",
  "swimlanes": [{ "id": "members", "name": "Members", "kind": "system" }],
  "events": {
    "member-joined": { "name": "Member Joined", "swimlaneId": "members", "aggregate": "member", "public": { "version": 1 },
      "fields": [{ "name": "memberId", "type": "string", "idAttribute": true }, { "name": "branchId", "type": "string" },
                 { "name": "email", "type": "string", "pii": true, "piiSubject": "memberId" }] }
  },
  "commands": {
    "join-member": { "name": "Join Member", "aggregate": "member",
      "fields": [{ "name": "memberId", "type": "string", "idAttribute": true }, { "name": "branchId", "type": "string" },
                 { "name": "email", "type": "string", "pii": true, "piiSubject": "memberId" }] }
  },
  "dataSubjects": { "erasure": { "roles": ["librarian"] } },
  "screens": { "desk": { "name": "Desk" } },
  "slices": [{ "id": "join", "name": "Join", "pattern": "stateChange", "swimlaneId": "members", "status": "created",
    "screenId": "desk", "commandId": "join-member", "eventIds": ["member-joined"], "scenarios": [] }]
}`

// archDocs writes a coordinating document with the members system and the
// lending one (receivedDoc's, public), and returns its path and lending's.
func archDocs(t *testing.T, edge string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("members.eventmodel.json", membersDoc)
	lending := write("lending.eventmodel.json", receivedDoc(`, "public": { "version": 1 }`))
	arch := write("library.architecture.json", `{
  "architectureSchemaVersion": "0.2.0", "id": "library", "name": "Library",
  "systems": {
    "members": { "name": "Members", "document": { "path": "members.eventmodel.json" } },
    "lending": { "name": "Lending", "document": { "path": "lending.eventmodel.json" } }
  },
  "edges": [`+edge+`]
}`)
	return arch, lending
}

const memberBecomesBorrower = `{ "id": "member-becomes-borrower", "kind": "internal",
  "from": { "system": "members", "event": "member-joined", "version": 1 },
  "to": { "system": "lending", "command": "register-borrower" },
  "targetId": "memberId", "fields": { "borrowerId": "memberId", "branchId": "branchId" },
  "examples": [{ "name": "a member becomes a borrower, their email stays",
    "given": { "memberId": "m-1", "branchId": "north", "email": "reader@example.org" },
    "then": { "targetId": "m-1", "fields": { "borrowerId": "m-1", "branchId": "north" } } }] }`

func edgesOf(t *testing.T, edge, system string) (*SystemEdges, error) {
	t.Helper()
	arch, lending := archDocs(t, edge)
	a, err := LoadArchitecture(arch)
	if err != nil {
		t.Fatal(err)
	}
	docPath := lending
	if system == "members" {
		docPath = filepath.Join(filepath.Dir(arch), "members.eventmodel.json")
	}
	doc, err := Load(docPath)
	if err != nil {
		t.Fatal(err)
	}
	return EdgesFor(a, system, doc, docPath, Options{})
}

// The receiving system gets the edge in, translated as the example says: the
// email never reaches it.
func TestTheEdgeIntoASystemIsReadAndItsExamplesHold(t *testing.T) {
	se, err := edgesOf(t, memberBecomesBorrower, "lending")
	if err != nil {
		t.Fatal(err)
	}
	if len(se.In) != 1 || se.In[0].Command != "RegisterBorrower" || se.In[0].Aggregate != "borrower" ||
		se.In[0].TargetField != "memberId" || len(se.Out) != 0 {
		t.Fatalf("got in %+v, out %+v", se.In, se.Out)
	}
	if len(se.Examples) != 1 || !se.Examples[0].Passed {
		t.Fatalf("the example did not hold: %+v", se.Examples)
	}
	src, err := scaffold.GenerateGoEdges("domain", se.Architecture, se.System, se.In, se.Out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(src, `Command: "RegisterBorrower", TargetField: "memberId"`) {
		t.Errorf("edges.go does not carry the edge:\n%s", src)
	}
}

// The sending system publishes the event with only the fields an edge maps.
func TestTheSenderPublishesOnlyMappedFields(t *testing.T) {
	se, err := edgesOf(t, memberBecomesBorrower, "members")
	if err != nil {
		t.Fatal(err)
	}
	if len(se.Out) != 1 || se.Out[0].EventType != "MemberJoined" ||
		strings.Join(se.Out[0].Fields, ",") != "branchId,memberId" {
		t.Fatalf("got out %+v", se.Out)
	}
}

func TestAnExampleThatDoesNotHoldIsRefused(t *testing.T) {
	wrong := strings.Replace(memberBecomesBorrower, `"then": { "targetId": "m-1", "fields": { "borrowerId": "m-1", "branchId": "north" } }`,
		`"then": { "targetId": "m-1", "fields": { "borrowerId": "m-1", "branchId": "north", "email": "reader@example.org" } }`, 1)
	_, err := edgesOf(t, wrong, "lending")
	if err == nil || !strings.Contains(err.Error(), `the command's fields are {"borrowerId":"m-1","branchId":"north"}`) {
		t.Fatalf("want the example refused, got %v", err)
	}
}

func TestAPersonalFieldOnAnEdgeIsRefused(t *testing.T) {
	pii := strings.Replace(memberBecomesBorrower, `"branchId": "branchId" }`, `"branchId": "email" }`, 1)
	_, err := edgesOf(t, pii, "members")
	if err == nil || !strings.Contains(err.Error(), `"email" is personal data`) {
		t.Fatalf("want the personal field refused, got %v", err)
	}
}

func TestADocumentThatIsNotTheSystemsIsRefused(t *testing.T) {
	arch, lending := archDocs(t, memberBecomesBorrower)
	a, err := LoadArchitecture(arch)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Load(lending)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EdgesFor(a, "members", doc, lending, Options{}); err == nil || !strings.Contains(err.Error(), "document in") {
		t.Fatalf("want the wrong document refused, got %v", err)
	}
}
