package emschema

import (
	"fmt"
	"sort"
)

// Boundary edges (schema 3.4.0-3.6.0) are how a system meets the outside
// world: an ingress, a delay or schedule that starts an automation, an
// effect an automation produces. They are described by the domain model but
// met by surface tooling, so this importer builds the domain model only. It
// accepts them, does not apply them, and says so: every one is listed in
// Document.LeftOut and so in the import report, never dropped silently.
//
// This is deliberately narrower than "ignore what is not understood": an
// unknown key anywhere is still refused (see supported.go), and so is any
// other newer feature (partitioning, selfAccess.via).

// leaveBoundaryEdges records every boundary edge in doc in doc.LeftOut and
// removes what cannot be checked without them: a scenario that waits for
// time (an elapsed step) would otherwise run as if no time had passed and
// report a result that means something else.
func leaveBoundaryEdges(doc *Document) {
	var out []string
	ids := make([]string, 0, len(doc.Ingresses))
	for id := range doc.Ingresses {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out = append(out, fmt.Sprintf("ingress %q: how the outside world reaches the system (schema 3.4.0); not built", id))
	}
	for i := range doc.Slices {
		s := &doc.Slices[i]
		if s.IngressID != "" {
			out = append(out, fmt.Sprintf("slice %q is started by ingress %q; the slice is generated, the ingress is not", s.ID, s.IngressID))
		}
		if len(s.Delay) > 0 {
			out = append(out, fmt.Sprintf("slice %q is started by a delay (schema 3.5.0); its command is generated, no reactor and no timer", s.ID))
		}
		if len(s.Schedule) > 0 {
			out = append(out, fmt.Sprintf("slice %q is started by a schedule (schema 3.5.0); its command is generated, no reactor and no timer", s.ID))
		}
		if len(s.Effect) > 0 {
			out = append(out, fmt.Sprintf("slice %q has an effect (schema 3.6.0); the effect is not built", s.ID))
		}
		kept := s.Scenarios[:0:0]
		for _, sc := range s.Scenarios {
			if waitsForTime(sc) {
				out = append(out, fmt.Sprintf("scenario %q of slice %q waits for time (an elapsed step, schema 3.5.0); it is not generated or checked", sc.ID, s.ID))
				continue
			}
			kept = append(kept, sc)
		}
		if len(kept) != len(s.Scenarios) {
			s.Scenarios = kept
		}
	}
	doc.LeftOut = out
}

func waitsForTime(sc Scenario) bool {
	for _, g := range sc.Given {
		if g.Elapsed != "" {
			return true
		}
	}
	return false
}
