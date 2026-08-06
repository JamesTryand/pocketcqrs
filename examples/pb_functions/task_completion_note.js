//@trigger reactor TaskCompleted
//@dispatches note/CreateNote
// Example JS reactor (tier 4): when a (Go) task is completed, dispatch a
// command that creates a note recording it, on the (JS) note aggregate.
// A saga on purpose spans two differently-implemented aggregates, to show
// that the reactor tier doesn't care where either side is defined.
//
// The target note id is derived from the source event, so an at-least-once
// replay dispatches the same command twice; the second hits "note already
// exists" on the note decider, which is logged and skipped, not resent.

function reactTo(event) {
	return [{
		aggregate: "note",
		id: "completed-" + event.aggregateId,
		command: "CreateNote",
		payload: { text: "task " + event.aggregateId + " was completed" },
	}];
}
