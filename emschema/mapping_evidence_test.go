package emschema

import "testing"

// Ports the two dotnetcqrs codegen fixes (v0.3.0 / v0.4.0) into the Go
// mapper: a scenario's `given` only counts against "this command is the
// aggregate's create" when a given event is on the slice's OWN aggregate
// stream, and a command with evidence for both a fresh and an existing
// stream is a genuine upsert (neither guard). Revised since: evidence
// detection is now an ORDERED FOLD of `given`
// tracking a synthesized `exists`, not a scan for own-stream presence — a
// `given` that assigns then unassigns (endsStream) nets back to "does not
// exist" and must count as create evidence, not update evidence.

func evidenceMapper(owners map[string]string, endsStream map[string]bool) *mapper {
	return &mapper{eventOwners: owners, eventEndsStream: endsStream}
}

func stateChangeScenario(given ...string) Scenario {
	sc := Scenario{Kind: KindStateChange}
	for _, id := range given {
		sc.Given = append(sc.Given, EventRef{EventID: id})
	}
	return sc
}

func TestHasCreateEvidence_crossAggregateGivenIsNotDisqualifying(t *testing.T) {
	// stage-shipment: a real create of `shipment`, whose only realistic
	// scenario needs cross-aggregate preconditions on other streams.
	m := evidenceMapper(map[string]string{
		"customer-created": "customer",
		"discount-set":     "discount",
		"shipment-staged":  "shipment",
	}, nil)
	slice := Slice{Scenarios: []Scenario{
		stateChangeScenario("customer-created", "discount-set"),
	}}
	if !m.hasCreateEvidence(slice, "shipment") {
		t.Fatal("a given made entirely of foreign-aggregate events must not disqualify create")
	}
	if m.hasUpdateEvidence(slice, "shipment") {
		t.Fatal("no given on shipment's own stream: there is no update evidence")
	}
}

func TestHasCreateEvidence_ownStreamGivenDisqualifies(t *testing.T) {
	m := evidenceMapper(map[string]string{
		"customer-created": "customer",
		"shipment-staged":  "shipment",
	}, nil)
	slice := Slice{Scenarios: []Scenario{
		stateChangeScenario("customer-created", "shipment-staged"),
	}}
	if m.hasCreateEvidence(slice, "shipment") {
		t.Fatal("a given event on shipment's own stream is evidence the stream already exists")
	}
	if !m.hasUpdateEvidence(slice, "shipment") {
		t.Fatal("a given event on the own stream is update evidence")
	}
}

func TestHasCreateEvidence_emptyGivenIsStillCreate(t *testing.T) {
	m := evidenceMapper(map[string]string{"thing-created": "thing"}, nil)
	slice := Slice{Scenarios: []Scenario{stateChangeScenario()}}
	if !m.hasCreateEvidence(slice, "thing") {
		t.Fatal("an empty given is the obvious create case")
	}
}

func TestEvidence_dualScenariosAreAnUpsert(t *testing.T) {
	// One "set" command, two scenarios: first-ever call (empty given) and a
	// "change an existing value" call (given seeds the own stream). Both
	// flags fall to false -> no existence guard, an upsert.
	m := evidenceMapper(map[string]string{"threshold-set": "notificationSettings"}, nil)
	slice := Slice{Scenarios: []Scenario{
		stateChangeScenario(),
		stateChangeScenario("threshold-set"),
	}}
	hasCreate := m.hasCreateEvidence(slice, "notificationSettings")
	hasUpdate := m.hasUpdateEvidence(slice, "notificationSettings")
	if !hasCreate || !hasUpdate {
		t.Fatalf("dual evidence expected: hasCreate=%v hasUpdate=%v", hasCreate, hasUpdate)
	}
	once := hasCreate && !hasUpdate
	requiresExisting := !hasCreate
	if once || requiresExisting {
		t.Fatalf("an upsert sets neither guard: once=%v requiresExisting=%v", once, requiresExisting)
	}
}

func TestEvidence_ignoresNonStateChangeScenarios(t *testing.T) {
	m := evidenceMapper(map[string]string{"thing-created": "thing"}, nil)
	errScenario := Scenario{Kind: KindError}
	slice := Slice{Scenarios: []Scenario{errScenario, stateChangeScenario("thing-created")}}
	if m.hasCreateEvidence(slice, "thing") {
		t.Fatal("only stateChange scenarios are evidence; the error scenario must be skipped")
	}
}

func TestHasCreateEvidence_reassignAfterUnassignNetsToCreate(t *testing.T) {
	// A supervisor reassigns a member after an unassign. given =
	// [assign, unassign] on the assignment's OWN stream. A SCAN would see an
	// own-stream event present and call this update evidence, which would
	// wrongly flip assign-member-to-team to an upsert (dropping the guard
	// against double-assigning a currently-active pair). The FOLD must net
	// this to "does not exist" -> create evidence, not update evidence.
	m := evidenceMapper(
		map[string]string{"member-assigned-to-team": "assignment", "member-unassigned-from-team": "assignment"},
		map[string]bool{"member-unassigned-from-team": true},
	)
	slice := Slice{Scenarios: []Scenario{
		stateChangeScenario("member-assigned-to-team", "member-unassigned-from-team"),
	}}
	if !m.hasCreateEvidence(slice, "assignment") {
		t.Fatal("a given ending on an endsStream event nets to \"does not exist\": this is create evidence")
	}
	if m.hasUpdateEvidence(slice, "assignment") {
		t.Fatal("a given that nets to \"does not exist\" must not also count as update evidence")
	}
}

func TestHasUpdateEvidence_plainOwnStreamGivenStillNetsToUpdate(t *testing.T) {
	// Regression: an ordinary own-stream given with no endsStream event
	// involved at all must still net to "exists" -> update evidence, exactly
	// as the pre-fold scan behaved.
	m := evidenceMapper(map[string]string{"customer-created": "customer", "shipment-staged": "shipment"}, nil)
	slice := Slice{Scenarios: []Scenario{
		stateChangeScenario("customer-created", "shipment-staged"),
	}}
	if m.hasCreateEvidence(slice, "shipment") {
		t.Fatal("a plain own-stream given with no endsStream event nets to \"exists\": not create evidence")
	}
	if !m.hasUpdateEvidence(slice, "shipment") {
		t.Fatal("a plain own-stream given with no endsStream event nets to \"exists\": update evidence")
	}
}

func TestScenarioNetExists_foreignEndsStreamEventIsIgnored(t *testing.T) {
	// An endsStream event belonging to a FOREIGN aggregate must not affect
	// this scenario's fold at all -- only own-stream events ever move
	// `exists`. Given: [own-stream create, foreign-aggregate endsStream].
	// The foreign event must be skipped entirely, leaving exists=true.
	m := evidenceMapper(
		map[string]string{
			"shipment-staged":      "shipment",
			"team-lead-unassigned": "teamLeadAssignment",
		},
		map[string]bool{"team-lead-unassigned": true},
	)
	slice := Slice{Scenarios: []Scenario{
		stateChangeScenario("shipment-staged", "team-lead-unassigned"),
	}}
	if m.hasCreateEvidence(slice, "shipment") {
		t.Fatal("a foreign-aggregate endsStream event must not reset this scenario's own-stream fold")
	}
	if !m.hasUpdateEvidence(slice, "shipment") {
		t.Fatal("exists should still net true: the foreign endsStream event is not evidence either way")
	}
}

func TestHasBeginAgainEvidence(t *testing.T) {
	// A create may begin its stream again only when a scenario shows
	// it doing so after its own stream ended.
	m := evidenceMapper(
		map[string]string{
			"lead-assigned": "assignment", "lead-unassigned": "assignment",
			"team-closed": "team",
		},
		map[string]bool{"lead-unassigned": true, "team-closed": true},
	)
	again := Slice{Scenarios: []Scenario{stateChangeScenario("lead-assigned", "lead-unassigned")}}
	if !m.hasBeginAgainEvidence(again, "assignment") {
		t.Fatal("assign after an unassign of the same stream is begin-again evidence")
	}
	fresh := Slice{Scenarios: []Scenario{stateChangeScenario()}}
	if m.hasBeginAgainEvidence(fresh, "assignment") {
		t.Fatal("a create on a fresh stream says nothing about beginning again")
	}
	foreign := Slice{Scenarios: []Scenario{stateChangeScenario("team-closed")}}
	if m.hasBeginAgainEvidence(foreign, "assignment") {
		t.Fatal("another aggregate's end is not this stream ending")
	}
	refused := Slice{Scenarios: []Scenario{{Kind: KindError, Given: []EventRef{{EventID: "lead-assigned"}, {EventID: "lead-unassigned"}}}}}
	if m.hasBeginAgainEvidence(refused, "assignment") {
		t.Fatal("an error scenario is not evidence")
	}
}
