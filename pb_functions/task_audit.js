//@trigger event TaskCreated TaskCompleted
// Built-in effect function: audit-log task lifecycle events.
// Effects are best-effort and may be non-deterministic; they must not
// write state (there is no write binding — changes go through commands).
console.log("[task_audit] " + event.type + " aggregate=" + event.aggregate +
	"/" + event.aggregateId + " seq=" + event.sequence + " data=" + JSON.stringify(event.data));
