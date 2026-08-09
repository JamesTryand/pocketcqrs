# Agent skills

Skills shipped with PocketCQRS for use with [Claude Code](https://claude.com/claude-code).
Claude Code discovers `.claude/skills/` automatically when you open this repo — or a project
that vendors it — so there is nothing to install.

| skill | for |
| --- | --- |
| [`pocketcqrs-domain`](pocketcqrs-domain/SKILL.md) | building and changing domains: JS deciders, projections, reactors, effect functions, the reload loop, and the mistakes that cost real time |

The skill deliberately **points at `docs/` rather than restating it**. Reference material
duplicated into a second place drifts from the first, and this project has already paid for
that more than once — a guide describing code that had moved, and a guide naming a registration
path that had been gated behind a flag. The skill carries the *workflow* and the *traps*, both
of which live nowhere else; everything factual stays in `docs/`, which is the single copy.

If you change behaviour that the skill describes, change the skill in the same commit — the
same rule `docs/contributing.md` sets for `docs/`.
