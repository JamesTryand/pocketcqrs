package emschema

import (
	"strings"
	"testing"
)

// receivedDoc: a borrower registered by another system (schema 6.1.0). The
// slice names neither a screen nor an ingress; its command is public.
func receivedDoc(public string) string {
	return `{
  "eventModelingSchemaVersion": "6.1.0",
  "id": "lending", "name": "Lending",
  "swimlanes": [{ "id": "lending", "name": "Lending", "kind": "system" }],
  "events": {
    "borrower-registered": { "name": "Borrower Registered", "swimlaneId": "lending", "aggregate": "borrower",
      "fields": [{ "name": "borrowerId", "type": "string", "idAttribute": true }, { "name": "branchId", "type": "string" }] }
  },
  "commands": {
    "register-borrower": { "name": "Register Borrower", "aggregate": "borrower"` + public + `,
      "fields": [{ "name": "borrowerId", "type": "string", "idAttribute": true }, { "name": "branchId", "type": "string" }] }
  },
  "slices": [{
    "id": "register", "name": "Register a borrower", "pattern": "stateChange", "swimlaneId": "lending",
    "status": "created", "commandId": "register-borrower", "eventIds": ["borrower-registered"],
    "scenarios": [{ "id": "registers", "name": "registers", "kind": "stateChange", "given": [],
      "when": { "commandId": "register-borrower", "data": { "borrowerId": "b-1", "branchId": "north" } },
      "then": { "events": [{ "eventId": "borrower-registered", "data": { "borrowerId": "b-1", "branchId": "north" } }] } }]
  }]
}`
}

// A public command with neither a screen nor an ingress is sent by another
// system: it imports, its command is generated, and the report says so.
func TestAReceivedCommandNeedsNoScreen(t *testing.T) {
	doc, err := Load(writeDoc(t, receivedDoc(`, "public": { "version": 1 }`)))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range mapped.Domains {
		for _, c := range d.Commands {
			found = found || c.Name == "RegisterBorrower"
		}
	}
	if !found {
		t.Error("RegisterBorrower was not generated")
	}
	if notes := strings.Join(mapped.Report.Warnings, "\n"); !strings.Contains(notes, `slice "register": register-borrower is sent by another system`) {
		t.Errorf("the report does not name the received command:\n%s", notes)
	}
}

// Neither a screen nor an ingress, and not public: nothing starts it.
func TestACommandNothingStartsIsRefused(t *testing.T) {
	doc, err := Load(writeDoc(t, receivedDoc("")))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Map(doc, Options{})
	if err == nil || !strings.Contains(err.Error(), `has no screenId or ingressId, and its command "register-borrower" is not public`) {
		t.Fatalf("want the slice refused as started by nothing, got %v", err)
	}
}

// Both a screen and an ingress: the schema refuses it, as before.
func TestAScreenAndAnIngressTogetherAreRefused(t *testing.T) {
	body := strings.Replace(receivedDoc(""), `"commandId": "register-borrower", "eventIds"`,
		`"screenId": "s", "ingressId": "i", "commandId": "register-borrower", "eventIds"`, 1)
	body = strings.Replace(body, `"slices": [`, `"screens": { "s": { "name": "S" } },
  "ingresses": { "i": { "name": "I" } },
  "slices": [`, 1)
	if _, err := Load(writeDoc(t, body)); err == nil {
		t.Fatal("a slice naming both a screen and an ingress was accepted")
	}
}
