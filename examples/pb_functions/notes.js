//@trigger projection notes on NoteCreated NoteTextChanged NoteArchived
//@schema notes noteId:text text:text archived:bool
//@key noteId
// Read side of the JS-defined note aggregate: one row per note.

function project(event) {
	if (event.type === "NoteCreated") {
		return { upsert: { key: event.aggregateId, fields: { text: event.data.text, archived: false } } };
	}
	if (event.type === "NoteTextChanged") {
		return { upsert: { key: event.aggregateId, fields: { text: event.data.text } } };
	}
	if (event.type === "NoteArchived") {
		return { upsert: { key: event.aggregateId, fields: { archived: true } } };
	}
}
