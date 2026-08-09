# Agent skills

Skills shipped with PocketCQRS for use with [Claude Code](https://claude.com/claude-code).

Claude Code discovers `.claude/skills/` automatically when you open **this repo**, so
contributors need do nothing. That does not reach the more common case — someone who ran
`go install` and writes functions in a directory of their own, with no clone anywhere. They
have the binary, so the binary carries the skills too:

```sh
pocketcqrs skill install                      # into ~/.claude/skills, every project
pocketcqrs skill install --dir .claude/skills # into one project
```

The copy in the binary is embedded from this directory at build time, so there is one source
of truth and it cannot drift from what you are reading.

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
