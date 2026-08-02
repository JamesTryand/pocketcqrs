# Note

A plain text note: created, edited, archived. The fully JS-defined vertical —
write side, read side and collection schema all come from `pb_functions/`
files; no Go code involved.

## Commands

| command | payload | intent | rejects when |
| --- | --- | --- | --- |
| `CreateNote` | `{ text: string }` | create the note | note already exists; `text` missing |
| `ChangeNoteText` | `{ text: string }` | edit the text | note does not exist; archived; `text` missing |
| `ArchiveNote` | `{}` | archive (terminal) | note does not exist; already archived |

## Events

| event | data | since version | notes |
| --- | --- | --- | --- |
| `NoteCreated` | `{ text: string }` | v1 | |
| `NoteTextChanged` | `{ text: string }` | v1 | |
| `NoteArchived` | `{}` | v1 | |

## Read models

| collection | owner | shape | notes |
| --- | --- | --- | --- |
| `notes` | JS projection `notes` (`pb_functions/notes.js`) | `noteId` (key), `text`, `archived` | collection created by the `//@schema` directive, no migration |

## Scenarios

- given nothing → `CreateNote {text:"a"}` → `NoteCreated`
- given `NoteCreated` → `ChangeNoteText {text:"b"}` → `NoteTextChanged`
- given `NoteCreated` → `ArchiveNote` → `NoteArchived`
- given `NoteCreated`, `NoteArchived` → `ArchiveNote` → rejected ("note already archived")
- given `NoteCreated`, `NoteArchived` → `ChangeNoteText` → rejected ("note is archived")

## Implementation

- decider: `pb_functions/note.js` (tier 3 — neutered VM: no `Math.random`,
  no `Date`, no `pb` bindings; `command.now` is the stamped time)
- projection: `pb_functions/notes.js` (tier 2)
- validation: the decider is dry-run validated at boot and on hot reload —
  a version that fails to fold existing history is refused and the previous
  one keeps serving
