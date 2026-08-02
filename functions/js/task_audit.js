// Built-in effect function: audit-log task lifecycle events.
// Effects are best-effort and may be non-deterministic; they must not
// write to guarded collections (the writeguard will reject them anyway).
(function () {
	// This source is evaluated once per event; keep it side-effect-only.
})();

console.log("[task_audit] " + event.type + " aggregate=" + event.aggregate +
	"/" + event.aggregateId + " seq=" + event.sequence + " data=" + JSON.stringify(event.data));
