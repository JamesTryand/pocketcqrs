// Package emschema reads EventModeling documents — the format defined by
// github.com/jamestryand/eventmodelschema — and maps them onto this
// project's intermediate domain model.
//
// The types below mirror eventmodeling.schema.json as read at tag v2.2.0
// (commit ba04be0, "Schema 2.2.0: derived read-model fields, stream-ending
// events, scoped queries", 2026-09-01). A vendored copy of that schema and
// its worked examples lives in testdata/eventmodelschema/, with the tag
// recorded in PROVENANCE.md.
//
// Two things about the source format shape everything here:
//
//   - It validates STRUCTURE ONLY. There is deliberately no referential
//     integrity check — JSON Schema has no cross-property lookup — so
//     nothing upstream verifies that a swimlaneId, eventId or readModelId
//     resolves. Lint does that here, because an importer that trusted the
//     ids would fail deep inside generation with a confusing error.
//   - Its version string is reported but NOT branched on. Upstream bumped to
//     2.0.0 for the removal of the `translation` pattern, which fixes the
//     signal going forward — but a document written before that bump declares
//     1.x while being a v2 document, indistinguishable by header from a real
//     v1 one. Generation is detected from the document's SHAPE; see
//     checkGeneration.
package emschema

import "encoding/json"

// Document is a whole EventModeling model.
//
// swimlanes and slices are ordered arrays because their position carries
// meaning (time flows left to right for slices; swimlane stacking is part of
// the diagram). Everything else is an id-keyed registry, which is what gives
// each element exactly one definition and one identity.
type Document struct {
	Schema                     string               `json:"$schema,omitempty"`
	EventModelingSchemaVersion string               `json:"eventModelingSchemaVersion"`
	ID                         string               `json:"id"`
	Name                       string               `json:"name"`
	Description                string               `json:"description,omitempty"`
	Swimlanes                  []Swimlane           `json:"swimlanes"`
	Chapters                   map[string]Chapter   `json:"chapters,omitempty"`
	ActorLanes                 map[string]Chapter   `json:"actorLanes,omitempty"`
	Events                     map[string]Event     `json:"events,omitempty"`
	Commands                   map[string]Command   `json:"commands,omitempty"`
	ReadModels                 map[string]ReadModel `json:"readModels,omitempty"`
	Screens                    map[string]Screen    `json:"screens,omitempty"`
	Automations                map[string]Chapter   `json:"automations,omitempty"`
	Slices                     []Slice              `json:"slices"`
	Hotspots                   map[string]Hotspot   `json:"hotspots,omitempty"`
}

// Swimlane is a team or system lane. Organisational, NOT an aggregate —
// deriving an aggregate from one would merge unrelated stream families.
type Swimlane struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"` // team | system
	Description string `json:"description,omitempty"`
}

// Chapter covers the three registries that are just {name, description}:
// chapters, actor lanes and automations. All the automation WIRING lives on
// the slice, not on the automation definition.
type Chapter struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Event is an event definition. Aggregate is optional and the worked example
// deliberately leaves boundary elements untagged.
type Event struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	SwimlaneID  string  `json:"swimlaneId"`
	Aggregate   string  `json:"aggregate,omitempty"`
	Fields      []Field `json:"fields,omitempty"`
	// EndsStream marks an event that closes its own aggregate's stream (an
	// unassign, a removal): folded, it resets the synthesized `Exists` to
	// false instead of true, so a later create-shaped command on the same
	// stream is legal again. Added in schema 2.2.0 (Finding 3).
	EndsStream bool `json:"endsStream,omitempty"`
}

// Command is a command definition. Reason is methodology prose with no
// runtime home here; it survives as a generated comment and in the domain
// doc, one-way.
type Command struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Reason      string  `json:"reason,omitempty"`
	Aggregate   string  `json:"aggregate,omitempty"`
	Fields      []Field `json:"fields,omitempty"`
}

// ReadModel has no Aggregate by design: read models are cross-cutting, so
// the owning aggregate is only derivable from BuiltFromEventIDs.
type ReadModel struct {
	Name              string   `json:"name"`
	Description       string   `json:"description,omitempty"`
	Question          string   `json:"question,omitempty"`
	BuiltFromEventIDs []string `json:"builtFromEventIds,omitempty"`
	Fields            []Field  `json:"fields,omitempty"`
	// Scopes declares, per non-column query param, how it resolves through
	// another read model (a semi-join) rather than a plain column filter.
	// Added in schema 2.2.0 (Finding 3).
	Scopes []ReadModelScope `json:"scopes,omitempty"`
}

// ReadModelScope declares how one query param resolves to a set of this
// read model's own key values via another read model, e.g. `pmStaffId` on
// `flagged-entries` resolves through `project-managers.staffId` to the
// `projectId` values a PM may see.
type ReadModelScope struct {
	Param string            `json:"param"`
	Via   ReadModelScopeVia `json:"via"`
}

// ReadModelScopeVia names the resolving read model and the fields the
// semi-join joins on: `matchParamTo` filters the via-model by the param
// value, `selectField` is what's collected from the matching rows, and
// `filterLocalField` is this read model's own field that must be in that
// collected set.
type ReadModelScopeVia struct {
	ReadModelID      string `json:"readModelId"`
	MatchParamTo     string `json:"matchParamTo"`
	SelectField      string `json:"selectField"`
	FilterLocalField string `json:"filterLocalField"`
}

// Screen is design-time notation with no runtime concept here — but it is
// REQUIRED on stateChange and stateView slices, so an export has to
// synthesize one rather than omit it.
type Screen struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ActorLaneID string `json:"actorLaneId,omitempty"`
}

// Field is a typed payload/column field. The type vocabulary is wider than
// this project's five //@schema types; see FoldType.
type Field struct {
	Name        string  `json:"name"`
	Type        string  `json:"type"`
	Description string  `json:"description,omitempty"`
	Optional    bool    `json:"optional,omitempty"`
	Cardinality string  `json:"cardinality,omitempty"` // single | list
	IDAttribute bool    `json:"idAttribute,omitempty"`
	PII         bool    `json:"pii,omitempty"`
	Subfields   []Field `json:"subfields,omitempty"`
	// Derivation makes a read-model field a fold over named events instead
	// of a same-named payload copy — a toggle, a count or a sum. Only
	// meaningful on a readModel field. Added in schema 2.2.0 (Finding 3).
	Derivation *FieldDerivation `json:"derivation,omitempty"`
}

// Derivation kinds, as the schema 2.2.0 discriminated union defines them.
const (
	DerivationToggle = "toggle"
	DerivationCount  = "count"
	DerivationSum    = "sum"
)

// FieldDerivation computes a read-model field as a fold over named events.
// Exactly one of the three shapes applies, selected by Kind; the fields
// belonging to the other two are left zero. This mirrors the source
// schema's `oneOf` on `fieldDerivation` — kept flat here, as EventRef/
// ReadModelQuery etc. already do for other kind-discriminated shapes in
// this file, rather than as three separate Go types the mapper would have
// to type-switch on.
type FieldDerivation struct {
	Kind string `json:"kind"`

	// toggle: OnEventIds sets the field true, OffEventIds sets it false.
	OnEventIDs  []string `json:"onEventIds,omitempty"`
	OffEventIDs []string `json:"offEventIds,omitempty"`
	Initial     bool     `json:"initial,omitempty"`

	// count: IncrementOnEventIds add 1, DecrementOnEventIds subtract 1.
	IncrementOnEventIDs []string `json:"incrementOnEventIds,omitempty"`
	DecrementOnEventIDs []string `json:"decrementOnEventIds,omitempty"`

	// sum: AddOnEventIds add AmountField's payload value,
	// SubtractOnEventIds subtract it.
	AddOnEventIDs      []string `json:"addOnEventIds,omitempty"`
	SubtractOnEventIDs []string `json:"subtractOnEventIds,omitempty"`
	AmountField        string   `json:"amountField,omitempty"`

	// RowKeyField names the payload field on the triggering events that
	// carries the target row's key (count/sum only — the events are
	// usually on a different stream than the read model's own, e.g. an
	// assignment event carrying the projectId it should roll up onto).
	// Defaults to the read model's own key-field name when empty.
	RowKeyField string `json:"rowKeyField,omitempty"`
}

// Slice is one vertical slice. The pattern-specific fields are flattened
// here rather than modelled as a union, because the source schema uses
// allOf+if/then with a single unevaluatedProperties — a property is only
// legal on the pattern whose branch declares it, which Lint enforces.
type Slice struct {
	ID                 string     `json:"id"`
	Name               string     `json:"name"`
	Pattern            string     `json:"pattern"` // stateChange | stateView | automation
	SwimlaneID         string     `json:"swimlaneId"`
	ChapterID          string     `json:"chapterId,omitempty"`
	BusinessCapability string     `json:"businessCapability,omitempty"`
	Status             string     `json:"status"`
	Scenarios          []Scenario `json:"scenarios"`

	// stateChange + stateView
	ScreenID string `json:"screenId,omitempty"`
	// stateChange
	CommandID string   `json:"commandId,omitempty"`
	EventIDs  []string `json:"eventIds,omitempty"`
	// stateView + automation (optional there)
	ReadModelID string `json:"readModelId,omitempty"`
	// automation
	AutomationID    string   `json:"automationId,omitempty"`
	TriggerEventIDs []string `json:"triggerEventIds,omitempty"`
	ResultEventIDs  []string `json:"resultEventIds,omitempty"`
}

// Scenario is a given/when/then example. The `when` and `then` shapes depend
// on Kind, so they stay raw until the kind selects one.
type Scenario struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Kind  string          `json:"kind"` // stateChange | stateView | error
	Given []EventRef      `json:"given"`
	When  json.RawMessage `json:"when,omitempty"`
	Then  json.RawMessage `json:"then,omitempty"`
}

// EventRef names an event and optionally carries example data.
type EventRef struct {
	EventID string          `json:"eventId"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// CommandRef names a command and optionally carries example data.
type CommandRef struct {
	CommandID string          `json:"commandId"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// ReadModelQuery is a stateView scenario's `when`.
type ReadModelQuery struct {
	ReadModelID string          `json:"readModelId"`
	QueryParams json.RawMessage `json:"queryParams,omitempty"`
}

// EventsThen is a stateChange scenario's `then`.
type EventsThen struct {
	Events []EventRef `json:"events"`
}

// ErrorThen is an error scenario's `then`.
type ErrorThen struct {
	Error struct {
		Code    string `json:"code,omitempty"`
		Message string `json:"message"`
	} `json:"error"`
}

// ResultThen is a stateView scenario's `then`.
//
// NOTE the open question recorded against the schema project: Result is a
// single object, but a read model answering a LIST question ("which orders
// are waiting to be shipped?") has no single-object answer.
type ResultThen struct {
	Result json.RawMessage `json:"result"`
}

// Hotspot is an open question pinned to part of the model. Design-time
// notation, known-lossy — but it is prose worth carrying into the domain
// doc on import.
type Hotspot struct {
	Note     string          `json:"note"`
	Target   json.RawMessage `json:"target"`
	Resolved bool            `json:"resolved,omitempty"`
}

// SchemaVersion is the generation this package targets, and what an export
// declares. 2.0.0 (2026-08-06) was the last breaking bump — removal of the
// `translation` pattern; 2.1.0 (2026-08-30) is additive only, a new
// `sliceStatus` value (`accepted`); 2.2.0 (2026-09-01) is additive only,
// `field.derivation`, `event.endsStream`, `readModel.scopes`.
const SchemaVersion = "2.2.0"

// Slice patterns and scenario kinds, as the v2 schema defines them.
const (
	PatternStateChange = "stateChange"
	PatternStateView   = "stateView"
	PatternAutomation  = "automation"

	KindStateChange = "stateChange"
	KindStateView   = "stateView"
	KindError       = "error"
)
