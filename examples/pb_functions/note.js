//@trigger decider note
//@handles NoteCreated NoteTextChanged NoteArchived
//@commands CreateNote ChangeNoteText ArchiveNote
// Example JS decider (tier 3): the full write side of a note aggregate,
// defined at runtime. This VM is neutered: no Math.random, no Date, no pb
// bindings — decide from command + state only. command.now is the time
// the registry stamped (recorded in the produced events' metadata).

function initialState() {
	return { exists: false, text: "", archived: false };
}

function decide(command, state) {
	switch (command.name) {
		case "CreateNote":
			if (state.exists) throw new Error("note already exists");
			if (!command.payload || !command.payload.text) throw new Error("text is required");
			return [{ type: "NoteCreated", data: { text: command.payload.text } }];

		case "ChangeNoteText":
			if (!state.exists) throw new Error("note does not exist");
			if (state.archived) throw new Error("note is archived");
			if (!command.payload || !command.payload.text) throw new Error("text is required");
			return [{ type: "NoteTextChanged", data: { text: command.payload.text } }];

		case "ArchiveNote":
			if (!state.exists) throw new Error("note does not exist");
			if (state.archived) throw new Error("note already archived");
			return [{ type: "NoteArchived", data: {} }];

		default:
			throw new Error("unknown command: " + command.name);
	}
}

function evolve(state, event) {
	if (event.type === "NoteCreated") {
		state.exists = true;
		state.text = event.data.text;
	} else if (event.type === "NoteTextChanged") {
		state.text = event.data.text;
	} else if (event.type === "NoteArchived") {
		state.archived = true;
	}
	return state;
}
