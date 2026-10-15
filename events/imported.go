package events

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ImportMarker is the metadata key ImportEvents sets on every event it
// brings in from another deployment: those events already happened there,
// side effects included. Effect-tier consumers (reactors, event-triggered
// functions, the external caller) fold them but don't act on them (see
// consumers.Effect); projections apply them as usual. It is set only by
// ImportEvents, and a native append carrying it is refused, so a native
// event can never be skipped as history.
const ImportMarker = "imported"

// ErrImportMarker refuses a native append whose metadata carries
// ImportMarker.
var ErrImportMarker = errors.New("events: metadata key \"" + ImportMarker + "\" is set only by an import")

// IsImported reports whether ev was brought in by ImportEvents.
func IsImported(ev Event) bool {
	var meta map[string]json.RawMessage
	if len(ev.Metadata) == 0 || json.Unmarshal(ev.Metadata, &meta) != nil {
		return false
	}
	v, ok := meta[ImportMarker]
	return ok && string(v) == "true"
}

// checkNotImported refuses native events whose metadata carries the
// marker, whatever its value.
func checkNotImported(evts []NewEvent) error {
	for _, ne := range evts {
		if hasImportMarker(ne.Metadata) {
			return fmt.Errorf("%w (type %s)", ErrImportMarker, ne.Type)
		}
	}
	return nil
}

func hasImportMarker(raw json.RawMessage) bool {
	var meta map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &meta) != nil {
		return false
	}
	_, ok := meta[ImportMarker]
	return ok
}

// markImported returns raw with ImportMarker set, keeping every other key.
func markImported(raw json.RawMessage) (json.RawMessage, error) {
	meta := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, fmt.Errorf("events: import: metadata is not a JSON object: %w", err)
		}
		if meta == nil {
			meta = map[string]json.RawMessage{}
		}
	}
	meta[ImportMarker] = json.RawMessage("true")
	return json.Marshal(meta)
}
