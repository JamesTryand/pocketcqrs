package emschema

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Schema 4.1.0 when.role: the role a command scenario's actor holds. Verify
// runs the command's authorization with it, as the gateway runs it with the
// caller's role: requiredRole and fieldGatedRole must admit it, and a
// requiredOwnership or scope whose bypassRoles name it is skipped. Without
// it, the actor holds the command's required roles and none of its bypass
// roles, as before. The document is upstream's state-ownership.json, whose
// approval scope a coordinator bypasses.

// stateOwnership is upstream's example as plain JSON, to change before
// verifying.
func stateOwnership(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(fixture(t, "examples/state-ownership.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func sliceOf(doc map[string]any, id string) map[string]any {
	for _, s := range doc["slices"].([]any) {
		if s.(map[string]any)["id"] == id {
			return s.(map[string]any)
		}
	}
	return nil
}

func addScenario(t *testing.T, doc map[string]any, sliceID, scenario string) {
	t.Helper()
	var sc any
	if err := json.Unmarshal([]byte(scenario), &sc); err != nil {
		t.Fatal(err)
	}
	s := sliceOf(doc, sliceID)
	s["scenarios"] = append(s["scenarios"].([]any), sc)
}

func verifyJSON(t *testing.T, doc map[string]any) map[string]ScenarioResult {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	loaded, mapped := loadMapped(t, writeDoc(t, string(raw)))
	results, err := Verify(loaded, mapped, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]ScenarioResult{}
	for _, r := range results {
		out[r.ScenarioID] = r
	}
	return out
}

func TestScenarioRoleInBypassRolesSkipsTheScope(t *testing.T) {
	r := verifyJSON(t, stateOwnership(t))["coordinator-approves-at-any-branch"]
	if !r.Passed {
		t.Fatalf("the coordinator's role bypasses the scope: %s", r.Detail)
	}
}

// The scenario must be able to fail: the role, not the actor, is what lets
// the coordinator through.
func TestWithoutTheRoleTheSameActorIsRefusedByTheScope(t *testing.T) {
	doc := stateOwnership(t)
	for _, sc := range sliceOf(doc, "approve-loan-request-slice")["scenarios"].([]any) {
		if sc.(map[string]any)["id"] == "coordinator-approves-at-any-branch" {
			delete(sc.(map[string]any)["when"].(map[string]any), "role")
		}
	}
	r := verifyJSON(t, doc)["coordinator-approves-at-any-branch"]
	if r.Passed || !strings.Contains(r.Detail, "the command's authorization refused it: only a volunteer at this loan request's branch may approve it") {
		t.Fatalf("without the role the scope refuses: passed=%v: %s", r.Passed, r.Detail)
	}
}

func TestScenarioRoleRequiredRoleDoesNotAdmitIsRefused(t *testing.T) {
	doc := stateOwnership(t)
	doc["commands"].(map[string]any)["approve-loan-request"].(map[string]any)["requiredRole"] = []any{"volunteer", "coordinator"}
	addScenario(t, doc, "approve-loan-request-slice", `{
		"id": "member-cannot-approve", "name": "A member, even one at the branch, cannot approve", "kind": "error",
		"given": [
			{ "eventId": "loan-requested", "data": { "loanRequestId": "r1", "memberId": "m1", "toolId": "t1", "branchId": "b1", "loanDays": 3 } },
			{ "eventId": "volunteer-joined-branch", "data": { "enrolmentId": "b1:v1", "branchId": "b1", "volunteerId": "v1" } }
		],
		"when": { "commandId": "approve-loan-request", "data": {}, "actor": "v1", "role": "member" },
		"then": { "error": { "message": "not authorized to issue ApproveLoanRequest on loanRequest" } }
	}`)
	results := verifyJSON(t, doc)
	if r := results["member-cannot-approve"]; !r.Passed || r.Detail != "refused by the command's authorization: not authorized to issue ApproveLoanRequest on loanRequest" {
		t.Errorf("member-cannot-approve: passed=%v: %s", r.Passed, r.Detail)
	}
	// without a role the volunteer holds the required role, as before 4.1.0;
	// the coordinator's role is admitted
	for _, id := range []string{"branch-volunteer-approves", "coordinator-approves-at-any-branch"} {
		if r := results[id]; !r.Passed {
			t.Errorf("%s: %s", id, r.Detail)
		}
	}
}

func TestScenarioRoleFieldGatedRoleChecksOnlyWhenItsConditionApplies(t *testing.T) {
	doc := stateOwnership(t)
	doc["commands"].(map[string]any)["request-loan"].(map[string]any)["fieldGatedRole"] = map[string]any{
		"field": "loanDays", "value": 30, "requiredRole": "coordinator",
	}
	addScenario(t, doc, "request-loan-slice", `{
		"id": "member-long-loan-refused", "name": "A member cannot ask for a 30-day loan", "kind": "error",
		"given": [], "when": { "commandId": "request-loan", "data": { "toolId": "t1", "branchId": "b1", "loanDays": 30 }, "actor": "m1", "role": "member" },
		"then": { "error": { "message": "not authorized to issue RequestLoan on loanRequest" } }
	}`)
	addScenario(t, doc, "request-loan-slice", `{
		"id": "coordinator-long-loan", "name": "A coordinator can", "kind": "stateChange",
		"given": [], "when": { "commandId": "request-loan", "data": { "toolId": "t1", "branchId": "b1", "loanDays": 30 }, "actor": "c1", "role": "coordinator" },
		"then": { "events": [{ "eventId": "loan-requested" }] }
	}`)
	addScenario(t, doc, "request-loan-slice", `{
		"id": "member-short-loan", "name": "A member's 3-day loan is not gated", "kind": "stateChange",
		"given": [], "when": { "commandId": "request-loan", "data": { "toolId": "t1", "branchId": "b1", "loanDays": 3 }, "actor": "m1", "role": "member" },
		"then": { "events": [{ "eventId": "loan-requested" }] }
	}`)
	results := verifyJSON(t, doc)
	for _, id := range []string{"member-long-loan-refused", "coordinator-long-loan", "member-short-loan"} {
		if r := results[id]; !r.Passed {
			t.Errorf("%s: %s", id, r.Detail)
		}
	}
}
