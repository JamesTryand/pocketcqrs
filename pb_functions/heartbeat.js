//@trigger cron * * * * *
// Example cron function: heartbeat with a read-side glimpse, every minute.
// Cron functions are effect-tier: the script body runs per tick, with a
// `job` global ({name, firedAt}) and the read-only pb binding.

var tasks = pb.query("tasks", "", 500) || [];
var notes = pb.query("notes", "", 500) || [];
console.log("[heartbeat] " + job.firedAt + " tasks=" + tasks.length + " notes=" + notes.length);
