package emschema

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jamestryand/pocketcqrs/catalog"
)

// exportSwimlane is the single lane an exported document declares.
//
// Swimlanes are ORGANISATIONAL — teams and systems — and a running platform
// knows nothing about how its authors were organised. But `swimlanes` has
// minItems:1, every event REQUIRES a swimlaneId and so does every slice, so
// omitting them produces an invalid document rather than a lossy one. One
// synthesized system lane is the honest minimum, and the report says it was
// invented.
const exportSwimlane = "system"

// FromCatalog renders a running platform's catalog as an EventModeling
// document.
//
// Export is a reconstruction, not a recovery: a catalog knows what exists,
// not how it was described. Everything synthesized or lost is named in the
// report rather than quietly filled in.
func FromCatalog(cat *catalog.Catalog) (*Document, *Report) {
	rep := &Report{}
	doc := &Document{
		Schema:                     "https://raw.githubusercontent.com/jamestryand/eventmodelschema/main/schema/eventmodeling.schema.json",
		EventModelingSchemaVersion: "1.0.0",
		ID:                         "pocketcqrs-export",
		Name:                       "PocketCQRS export",
		Description: "Reconstructed from a running PocketCQRS platform's catalog. " +
			"Design-time notation (swimlanes, screens, chapters, actor lanes, hotspots, " +
			"slice status) has no runtime counterpart and was synthesized or omitted; " +
			"see the import report for the full list.",
		Swimlanes: []Swimlane{{
			ID: exportSwimlane, Name: "System", Kind: "system",
			Description: "Synthesized: a running platform has no record of how its authors were organised.",
		}},
		Events:      map[string]Event{},
		Commands:    map[string]Command{},
		ReadModels:  map[string]ReadModel{},
		Screens:     map[string]Screen{},
		Automations: map[string]Chapter{},
	}
	rep.warnf("swimlanes are not a runtime concept; one %q lane was synthesized because "+
		"every event and every slice requires one", exportSwimlane)

	eventsByAggregate := exportEvents(cat, doc, rep)
	exportStateChangeSlices(cat, doc, rep, eventsByAggregate)
	exportReadModels(cat, doc, rep, eventsByAggregate)
	exportAutomations(cat, doc, rep, eventsByAggregate)

	rep.lossyf("chapters, actor lanes, hotspots and slice status are design-time notation " +
		"with no runtime counterpart; none are emitted")
	rep.lossyf("Command.reason and ReadModel.question are methodology prose; the runtime " +
		"does not carry them, so an exported document has none unless it is edited back in")
	return doc, rep
}

// exportEvents registers every event type the platform knows about, declared
// or observed, and returns them grouped by aggregate.
func exportEvents(cat *catalog.Catalog, doc *Document, rep *Report) map[string][]string {
	byAggregate := map[string][]string{}
	for _, agg := range cat.Aggregates {
		seen := map[string]bool{}
		add := func(typeName string) {
			if typeName == "" || seen[typeName] {
				return
			}
			seen[typeName] = true
			id := DeriveID(typeName)
			doc.Events[id] = Event{
				Name:       DeriveName(typeName),
				SwimlaneID: exportSwimlane,
				Aggregate:  agg.Name,
			}
			byAggregate[agg.Name] = append(byAggregate[agg.Name], id)
		}
		// declared first, then empirical: //@handles is the contract, the
		// log is the evidence, and a type in the log that is not declared
		// is worth carrying rather than dropping
		for _, h := range agg.Handles {
			add(h)
		}
		for _, e := range agg.Events {
			add(e.Type)
		}
		sort.Strings(byAggregate[agg.Name])
	}
	return byAggregate
}

// exportStateChangeSlices emits one slice per declared command.
func exportStateChangeSlices(cat *catalog.Catalog, doc *Document, rep *Report, eventsByAggregate map[string][]string) {
	var widened int
	for _, agg := range cat.Aggregates {
		events := eventsByAggregate[agg.Name]
		if len(agg.Commands) == 0 {
			rep.warnf("aggregate %q declares no commands (no //@commands or Commands field), so it "+
				"contributes no stateChange slice: commands leave no trace in the log and cannot be "+
				"recovered empirically", agg.Name)
			continue
		}
		if len(events) == 0 {
			rep.warnf("aggregate %q has no events, declared or observed, so its commands cannot form "+
				"a valid slice (eventIds needs at least one)", agg.Name)
			continue
		}
		for _, cmdName := range agg.Commands {
			cmdID := DeriveID(cmdName)
			doc.Commands[cmdID] = Command{
				Name:      DeriveName(cmdName),
				Aggregate: agg.Name,
			}
			// //@produces is what makes a faithful slice possible: it names
			// the events THIS command appends. Without it the association is
			// unrecoverable and the slice has to list everything the
			// aggregate can emit.
			sliceEvents := events
			if produced, ok := agg.Produces[cmdName]; ok && len(produced) > 0 {
				sliceEvents = nil
				for _, t := range produced {
					evID := DeriveID(t)
					if _, known := doc.Events[evID]; known {
						sliceEvents = append(sliceEvents, evID)
					}
				}
				sort.Strings(sliceEvents)
			}
			if len(sliceEvents) == 0 {
				rep.warnf("command %q declares //@produces events this export does not know; "+
					"falling back to the aggregate's whole event set", cmdName)
				sliceEvents = events
			} else if len(agg.Produces[cmdName]) == 0 {
				widened++
			}
			// A SCREEN IS REQUIRED on a stateChange slice, so it is
			// synthesized — omitting it produces an invalid document. Note
			// this is emitted ONLY for stateChange and stateView: the
			// source schema is allOf+if/then under one
			// unevaluatedProperties, so a screenId on an automation slice
			// would be rejected outright.
			screenID := cmdID + "-screen"
			doc.Screens[screenID] = Screen{Name: DeriveName(cmdName)}

			doc.Slices = append(doc.Slices, Slice{
				ID:         cmdID + "-slice",
				Name:       DeriveName(cmdName),
				Pattern:    PatternStateChange,
				SwimlaneID: exportSwimlane,
				Status:     "informational",
				ScreenID:   screenID,
				CommandID:  cmdID,
				EventIDs:   sliceEvents,
				Scenarios:  []Scenario{},
			})
		}
	}
	if widened > 0 {
		// The honest statement of the biggest reconstruction gap. Nothing in
		// the runtime records WHICH command produces WHICH event: //@commands
		// names the commands, //@handles names the events, and no directive
		// links a pair. So each slice lists the aggregate's whole event set.
		rep.warnf("%d stateChange slice(s) list their aggregate's ENTIRE event set because their "+
			"decider declares no //@produces. //@commands and //@handles each name one side and "+
			"nothing joins a pair, so the association cannot be recovered without it and a round "+
			"trip widens eventIds. Add //@produces <Command> <Event...> to close the gap", widened)
	}
}

// exportReadModels turns projection consumers into read models and their
// stateView slices.
func exportReadModels(cat *catalog.Catalog, doc *Document, rep *Report, eventsByAggregate map[string][]string) {
	for _, cons := range cat.Consumers {
		if cons.Kind != "js-projection" && cons.Kind != "go-projection" {
			continue
		}
		for _, collection := range cons.Collections {
			id := DeriveID(TypeName(collection, collection))
			var builtFrom []string
			for _, t := range cons.EventTypes {
				evID := DeriveID(t)
				if _, ok := doc.Events[evID]; ok {
					builtFrom = append(builtFrom, evID)
				}
			}
			sort.Strings(builtFrom)
			doc.ReadModels[id] = ReadModel{
				Name:              DeriveName(TypeName(collection, collection)),
				BuiltFromEventIDs: builtFrom,
			}
			screenID := id + "-screen"
			doc.Screens[screenID] = Screen{Name: DeriveName(TypeName(collection, collection))}
			doc.Slices = append(doc.Slices, Slice{
				ID:          id + "-view-slice",
				Name:        DeriveName(TypeName(collection, collection)),
				Pattern:     PatternStateView,
				SwimlaneID:  exportSwimlane,
				Status:      "informational",
				ScreenID:    screenID,
				ReadModelID: id,
				Scenarios:   []Scenario{},
			})
			if len(builtFrom) == 0 {
				rep.warnf("read model %q lists no source events: its projection declares triggers "+
					"this export could not match to a known event type", id)
			}
		}
	}
}

// exportAutomations turns JS reactors into automation slices.
//
// Declared dispatches are what make this possible at all. A reactor that has
// never fired produces no flow edge, so the empirical log cannot show it —
// //@dispatches is the only record, exactly as //@commands is for a decider.
func exportAutomations(cat *catalog.Catalog, doc *Document, rep *Report, eventsByAggregate map[string][]string) {
	for _, cons := range cat.Consumers {
		if cons.Kind != "js-reactor" {
			continue
		}
		name := strings.TrimPrefix(cons.Name, "fn-reactor:")
		id := DeriveID(TypeName(name, name))
		if len(cons.Dispatches) == 0 {
			rep.warnf("reactor %q declares no //@dispatches, so the command it sends cannot be "+
				"named and it contributes no automation slice", name)
			continue
		}
		var triggers []string
		for _, t := range cons.EventTypes {
			evID := DeriveID(t)
			if _, ok := doc.Events[evID]; ok {
				triggers = append(triggers, evID)
			}
		}
		if len(triggers) == 0 {
			rep.warnf("reactor %q triggers on event types this export does not know, so it "+
				"contributes no automation slice", name)
			continue
		}
		sort.Strings(triggers)

		// one slice per declared dispatch: the schema's automation slice
		// carries exactly one commandId
		for _, d := range cons.Dispatches {
			aggName, cmdName, ok := strings.Cut(d, "/")
			if !ok {
				continue
			}
			cmdID := DeriveID(cmdName)
			if _, exists := doc.Commands[cmdID]; !exists {
				doc.Commands[cmdID] = Command{Name: DeriveName(cmdName), Aggregate: aggName}
			}
			results := eventsByAggregate[aggName]
			if len(results) == 0 {
				rep.warnf("reactor %q dispatches %s but that aggregate has no events, so the "+
					"automation slice would be invalid (resultEventIds needs at least one)", name, d)
				continue
			}
			doc.Automations[id] = Chapter{
				Name:        DeriveName(TypeName(name, name)),
				Description: "Reconstructed from a JS reactor's declared triggers and dispatches.",
			}
			doc.Slices = append(doc.Slices, Slice{
				ID:              id + "-automation-slice",
				Name:            DeriveName(TypeName(name, name)),
				Pattern:         PatternAutomation,
				SwimlaneID:      exportSwimlane,
				Status:          "informational",
				AutomationID:    id,
				TriggerEventIDs: triggers,
				CommandID:       cmdID,
				ResultEventIDs:  results,
				Scenarios:       []Scenario{},
				// NO ScreenID here: the source schema's unevaluatedProperties
				// would reject it on an automation slice.
			})
		}
	}
}

// Marshal renders a document as the JSON the schema validates, with slices
// in a stable order so two exports of the same platform are comparable.
func Marshal(doc *Document) ([]byte, error) {
	sort.SliceStable(doc.Slices, func(i, j int) bool { return doc.Slices[i].ID < doc.Slices[j].ID })
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("emschema: rendering the document: %w", err)
	}
	return append(raw, '\n'), nil
}
