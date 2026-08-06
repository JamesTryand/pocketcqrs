//@trigger http
// Example HTTP function: GET/POST /api/fn/hello
// Demonstrates the request object and the read-only pb binding.

function handle(request) {
	var name = (request.query && request.query.name) || (request.body && request.body.name) || "world";

	// read-side access example: count tasks via the query binding
	var tasks = pb.query("tasks", "", 500);

	return {
		status: 200,
		body: {
			message: "hello, " + name,
			method: request.method,
			taskCount: tasks ? tasks.length : 0,
			actor: request.auth ? request.auth.id : null
		}
	};
}
