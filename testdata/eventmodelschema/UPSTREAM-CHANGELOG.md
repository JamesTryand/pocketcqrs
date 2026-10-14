# Changelog

Tracks `eventModelingSchemaVersion` releases of `schema/eventmodeling.schema.json`.
See `docs/design-notes.md` for the full rationale behind each change.

## 3.17.0

**Additive (non-breaking):**
- `readModel.projection`: `"generated"` (the default) or `"hand"`, a read model built by a hand-written projection
  because no declaration expresses it (a join across streams, say). A generator still generates its query route; a
  check running only generated code reports its view scenarios as needing a host (HAND), and a host check runs the
  host's own projection against them.
- `examples/hand-projection.json`: orders placed after their day's dispatch closed.

## 3.16.0

**Additive (non-breaking):**
- `readModel.rowsPerListItem` (`listField`, `groupBy`): one row per group of a stream's list items, in place of one
  row per stream. Each carrying event replaces the stream's list; an item's `groupBy` subfield names its group; a
  row is keyed `<stream>|<group>` and holds the group's items, the group's value, and the stream's other fields.
  An item without the subfield is in no row. A `removedByEventIds` event removes all of the stream's rows. Not with
  `rowKeyField`.
- `derivation.kind: "sumOfItems"` (`subfield`), with `rowsPerListItem` only: the sum of a subfield over the row's
  items, absent unless every item carries it.
- `derivation.kind: "byEvent"` (`values`): a literal per event, such as a status (`toggle` is the 0/1 case). An
  event not named leaves the field as it was.
- `examples/rows-per-list-item.json`: an invoice's parts per project.

## 3.15.0

**Additive (non-breaking):**
- `requiredOwnership.stateField`: the field of the aggregate's own state (folded from its own events) that names
  the owner. The aggregate's decision compares it with the command's actor and refuses anyone else, as forbidden
  rather than invalid, with `message` if given. No read model is consulted, so the first command after a create is
  never refused while a projection catches up, and a change of owner takes effect at once.
- `requiredOwnership` is now either `via` (a read model, as before, with optional `bypassRoles`) or `stateField`
  (with optional `message`), never both. `bypassRoles` stays with `via`: a decision knows its actor, not their
  role.
- `examples/state-ownership.json`: an expense changed only by its own staff member.

## 3.14.0

**Additive (non-breaking):**
- `readModel.rowKeyField`: the field of each event a read model is built from (and removed by) that names the row
  it updates, in place of the event's stream. Without it, a row is a stream, as before.
- The use: one stream that holds many rows' events, such as a ledger recording a payment per person per run. Every
  one of those events must carry the field. `examples/repeated-outcomes.json` gains a `payments` read model, one row
  per payment.

## 3.13.0

**Additive (non-breaking):**
- An alternative in `outcomes` (the object form) may list `repeated` events: events that occur **zero or more
  times**, one per item, each in its place in `eventIds`' order. Each must be one of the alternative's `eventIds`.
- How many, and their values, are hand-written: a generator emits a hook for them, as for `when: "hand"`.
- The use: one decision that records something for each of several people, where each person's data must be its
  own event (a PII value is encrypted under one subject's key). New example `examples/repeated-outcomes.json`.

## 3.12.0

**Additive (non-breaking):**
- An alternative in `outcomes` may be an object, `{ "eventIds", "description"?, "when"?, "set"? }`,
  as well as a list of ids (3.3.0's form, unchanged in meaning).
- `when` says when the alternative applies:
  - a list of `{ "all": [conditions] }` groups, with the conditions of `refuseWhen`: it holds when any group
    holds;
  - or `"hand"`: the condition is hand-written, because it depends on a calculation the model doesn't describe.
    A generator emits a hook for it rather than a rule.
- When any alternative has a `when`, the alternatives are tried in order after the command's refusals; the first
  whose `when` holds is the result, and the last has none (the default). So the same command and state always
  give the same events.
- `set` fixes field values on an alternative's events, by event id then field (`{ "entry-flagged": { "reason":
  "long-day" } }`).
- New example `examples/conditional-outcomes.json`, with a declared condition and a hand-written one.
- Raised by `project/timesheets` D30.18 (F15): an entry whose hours are paid at no cost is logged and flagged in
  one decision. 3.3.0 could list the two alternatives but not say which applies, so generators refused it.

**Not enforced here:** that the last alternative has no `when`, that the ids in `eventIds` and `set` belong to
the slice, and that each field named exists. Reference checks, for a generator or a lint.

## 3.11.0

**Additive (non-breaking):**
- A `refuseWhen` condition gains four numeric operators: `lessThan`, `atMost`,
  `greaterThan` and `atLeast`. Each compares the field with a number, or with the value of
  another numeric `state` or `command` field.
- A comparison with an unset value never holds. A rule that also refuses a missing value
  says so with a separate `is: notSet` rule.
- `examples/refusals.json` uses them: "hours must be positive" is `atMost: 0` (it was
  `equals: 0`, which let negative hours through), and a new rule refuses more than 24.
- Raised by `project/timesheets` D30 (F10-F12): a conversion rate must be more than 0, and
  a VAT percentage from 0 to 100. As hand-written guards, the generated stand-ins couldn't
  see them.

## 3.10.0

**Additive (non-breaking):**
- `command` gains `refuseWhen`: rules that refuse the command. Each rule has a `message`
  and `all`, a list of conditions that must all hold for it to refuse. Rules are checked
  in order, after the create and exists checks. The first rule that holds refuses the
  command with its message. A scenario of kind `error` names that message.
- A condition reads one field: `state` (a field of the aggregate's state, folded from
  its own events) or `command` (a field of the command). It applies one operator:
  - `is`: `set` or `notSet`;
  - `equals` or `notEquals`: a literal, or `{ "state" | "command": field }`;
  - `contains`: a list field holds a literal, or the value of another field (so a
    stream can refuse an id it has already recorded).
- Rules see only the aggregate's own stream and the command. A rule that needs another
  aggregate's state belongs in a process manager or a stream that owns it, never in a
  read model on the write path.
- **Generators must refuse what they can't apply.** A runtime that cannot generate a
  declared refusal must refuse to generate, never drop it.
- A command scenario's `when` may name its `actor` and the time it happens (`at`), for a
  harness that runs a host's own deciders: they record who and when on events, so a
  worked example that checks values has to fix both.
- New example `examples/refusals.json`: an entry claimed by one invoice at a time, and
  frozen while claimed. The other examples are bumped.
- Raised by `project/timesheets` (decision D29, 2026-10-10). Every guard of this kind was
  hand-written, so the scenario harness (which runs the generated decider) reported
  them all as failures.

## 3.9.0

**Additive (non-breaking):**
- `readModel` gains `removedByEventIds`: events that each remove the row they target. A
  row is targeted the way the read model's seed events target it, by the event's own
  stream id. A later seed event for that stream creates the row again. Each id should
  also be in `builtFromEventIds`.
- Without it, nothing could say "this event takes the row away". `endsStream` (2.2.0) is
  about the write side's `Exists`, not read models. So every runtime hand-wrote the
  delete, or forgot to.
- Example `read-access.json`: unassigning a project manager removes the row, and with it
  the access the row granted (a 3.8.0 granting scope through that read model).
- Raised by `project/timesheets` (decision D23, 2026-10-09). An unassigned staff member
  kept their project-staff row, and with it access to the project's documents. Four other
  projections there already hand-wrote this delete.

## 3.8.0

**Additive (non-breaking):**
- `readModelScope` gains `grantsAccess` (boolean, default `false`). When it is `true`,
  the scope's param always names the caller: its value is the caller's own subject id,
  and whatever value the request sent is ignored. A caller who does not hold the read
  model's `requiredRole` may also see the rows this scope admits for them. "The
  entries on projects I manage" is the example: a scope through a project-managers
  read model.
- `readModelSelfAccess` gains `param`, an optional query param. When a request carries
  it, any caller is narrowed to their own rows, role holders included, and its value is
  ignored. Without it, a manager on their own "my time" page sees everyone's rows,
  because holding `requiredRole` shows every row.
- `readModelQuery` (a stateView scenario's `when`) gains `caller`: `{ "subjectId",
  "role"? }`, the caller the scenario reads as. Leaving it out keeps the old meaning: a
  caller who holds `requiredRole`.
- **The access rule, now stated whole:**
  - A caller holding `requiredRole` sees every row; params only narrow.
  - Any other signed-in caller sees the rows whose `selfAccess.subjectField` is their
    subject id, together with the rows each `grantsAccess` scope admits for them;
    params narrow within that.
  - A read model with neither `selfAccess` nor a `grantsAccess` scope refuses them, as
    before.
  - A read model without `requiredRole` but with `selfAccess` or a `grantsAccess` scope
    has no role holders, so every caller gets the second rule (as 3.2.0 already said for
    `selfAccess`). A read model that declares none of the three is unchanged: every
    signed-in caller sees every row.
- **Generators must fail closed.** A runtime that cannot apply a declared access rule
  must refuse to generate, or refuse the query. It must never serve the rows unscoped.
- New example `examples/read-access.json`; the order-fulfillment examples are bumped.
- Raised by `project/timesheets` (decision D21, 2026-10-09). Its hand-written routes
  scoped a staff caller only when the client sent the scope param, so a caller who left
  the param out read everyone's rows.

## 3.7.1

**Clarification (no schema shape change):**
- **`dataSubjects.erasure` authorises a request for erasure, never erasure itself.**
  3.2.0 described it as saying who may call the runtime's `EraseSubject`. Runtimes now
  govern erasure: a request is checked against the system's retention duties and can
  be held (or blocked by a legal hold) before the key is destroyed. So `self` and
  `roles` authorise the command that *starts* that process (`RequestErasure` in
  dotnetcqrs). Holding, approving, legal holds and direct erasure (`EraseSubject`,
  which skips the retention check) are host policy and are never declared in a
  document. Read the old way, `self: true` would have let a person destroy their own
  key the moment they asked. A system with nothing to retain approves requests at
  once, so "delete my account" behaves the same there.
- **A data subject belongs to one partition.** With `partitioning`, the same person in
  two partitions (or in two systems) is two data subjects, each with its own key and
  its own erasure process. Systems that share data pass an erasure on by sending the
  other system a request, which its own retention check then rules on; they never
  share keys or erase each other's data directly.
- Documents need no change: anything valid under 3.7.0 is valid under 3.7.1. The
  `eventModelingSchemaVersion` default and both order-fulfillment examples are bumped.

## 3.7.0

**Additive (non-breaking):**
- New top-level `partitioning`: `{ "key", "description"?, "crossPartitionRoles"? }`.
  When it is present, every stream and every read-model row belongs to one
  partition, unless the element opts out. Tenancy is the usual case: each tenant is
  a partition.
  - `key` names the partition key (for example `tenantId`). It lives in each event's
    metadata, not in `fields`, so no element can forget to carry it.
  - A command runs in the partition of whatever sent it: the caller (resolved by the
    host, like the actor id), an ingress (resolved by the host's adapter), the event
    that triggered an automation or started a timer, or the read-model row a schedule
    is working through.
  - A partitioned read model returns only the caller's partition. Roles in
    `crossPartitionRoles` (an operator, say) see every partition. `requiredRole`,
    `selfAccess`, `scopes` and `filters` then narrow the result as before.
- Events, commands and read models accept `partitioned: false` to opt out, for things
  that genuinely span partitions (the tenants themselves, shared infrastructure).
- Automation slices accept `partitionFrom`: a field of the trigger event naming the
  partition, for when a partitioned command is started by something that has no
  partition (a global event).
- Without `partitioning`, nothing changes. Split documents carry `partitioning` inline
  in the manifest; `manifest.schema.json`, `split.js` and `join.js` are updated.
- Raised by `project/container-paas` and `project/landing-pages` (decision 0006:
  tenancy in the data model from day one, because adding it after events exist is a
  migration).

**Examples:** `order-fulfillment` is partitioned by `shopId`, with `operator` able to
read across shops. Both order-fulfillment examples are bumped to `3.7.0`.

**Not enforced here:** that a command and its events agree on `partitioned`, that
`partitionFrom` names a field of the trigger event, that a partitioned command is never
started from a context with no partition and no `partitionFrom`, and that no field
shares the key's name. Checks for a lint. `partitioned` and `partitionFrom` are
accepted, and mean nothing, in a document without `partitioning`.

## 3.6.0

**Additive (non-breaking):**
- Automation slices gain `effect`: the automation does something in the outside world
  (calls an API, starts a container, sends an email) before reporting back. It has a
  required `swimlaneId` (the external system), an optional `description`, and a
  required `gaveUp`.
  - The slice's own `commandId` and `resultEventIds` report **success**: the command
    the runtime sends once the outside world has answered.
  - `gaveUp` (`commandId`, `resultEventIds`, optional `outcomes`) reports that the
    runtime **stopped trying**. Failing and timing out are one outcome here, with the
    reason in the command's data, since the domain rarely treats them differently.
  - The request itself needs no new element: it is the trigger event (or timer, or
    schedule) that started the automation.
- Timeouts, retries, backoff and attempt tracking are **not** in the document. The
  runtime owns them, and the host configures them. Any lifecycle records the runtime
  keeps (attempts, timeouts) are its own, like the `DataSubject` stream.
- Raised by `project/container-paas`: its deploy pipeline is mostly calls that take
  seconds to minutes and can succeed, fail or never answer, and the schema had no way
  to say an automation calls out at all.

**Examples:** `notify-shipping-partner-slice` is now an effect. Its success command is
renamed `record-partner-notified` (from `notify-shipping-partner`), and it gives up with
`record-partner-notification-abandoned` → `partner-notification-abandoned`, with a
scenario for each. Both order-fulfillment examples are bumped to `3.6.0`.

**Not enforced here:** that `effect.swimlaneId` names a `system` swimlane, and that the
`gaveUp` ids exist. Reference checks for a lint.

## 3.5.0

**Additive (non-breaking):**
- Automation slices can be started by time. Either way, the result is still a
  command that a decider can reject.
  - **`delay`** (with `triggerEventIds`): each trigger event starts a timer, and
    when it goes off the automation runs. `after` gives a duration, or `at` names a
    field of the trigger event holding the time. By default the timer belongs to the
    trigger's stream; `key` names a field to attach it to instead, such as
    `customerId`. A later trigger with the same key restarts the timer, and any of
    `cancelledByEventIds` with the same key cancels it.
  - **`schedule`** (instead of `triggerEventIds`): `cron`, a five-field cron
    expression, and an optional `timeZone` (IANA name, default `UTC`). With
    `readModelId`, the command is sent once per row; without, once per tick.
  - An automation slice now needs exactly one of `triggerEventIds` and `schedule`.
    `delay` needs `triggerEventIds`.
- Durations are ISO 8601, limited to weeks, days, hours, minutes and whole seconds
  (`P5D`, `PT30M`, `P1DT2H`). A day is exactly 24 hours. Months and years are left
  out, because "a month later" needs calendar rules.
- A duration or cron expression can be a literal, or
  `{ "setting": "<name>", "default": <value> }`. The host can change a named setting
  without changing the document, and the default keeps the document runnable as
  written.
- Scenarios: `given` can contain `{ "elapsed": "<duration>" }` between events,
  meaning that much time passes there. This works in any scenario, since a decider
  guarding on time needs it too.
- Raised by `project/landing-pages` (checkout expiry, drip sequences) and
  `project/container-paas` (certificate renewal, reconcile loops).

**Examples:** `delivery-overdue-slice` flags an order not delivered or failed
`deliveryOverdueAfter` (default `P5D`) after shipping, with an `elapsed` scenario.
`reconcile-shipments-slice` retries shipping every row of `pending-shipments` on
`shipmentReconcileCron` (default every 15 minutes). Both order-fulfillment examples
are bumped to `3.5.0`.

**Not enforced here:** that `at`, `key` and the cancelling events' key field name
fields of those events, and that a cron expression is valid beyond having five
fields. Checks for a lint or the host.

## 3.4.0

**Additive (non-breaking):**
- New top-level registry `ingresses`: a place where a third party (a payment
  processor, a carrier, a certificate authority) calls into the system. It is the
  machine counterpart of a `screen`. Each ingress has a `name`, an optional
  `description` and `swimlaneId` (the external system's lane), and two required
  properties:
  - `verification.scheme`: the name of the signature scheme every call must pass,
    such as `hmac-sha256`. The host maps the name to a verifier and holds the secret;
    a secret can never be written into a document.
  - `deliveryIdField`: the command field holding the sender's own id for the call.
    Senders redeliver, so a repeated id is answered from the first result and never
    dispatched twice.
- A `stateChange` slice now names exactly one entry point: `screenId` or
  `ingressId`. Before 3.4.0 `screenId` was required, so a command called by a third
  party had no slice it could belong to.
- Hotspots can target an `ingress`. Split documents keep `ingresses` in their own
  registry file; `manifest.schema.json` and `split.js` are updated.
- Raised by `project/landing-pages` (payment-processor webhooks) and
  `project/container-paas` (provider callbacks).

**Examples:** a `carrier-webhook` ingress from the shipping partner feeds a new
`record-delivery-slice`: `record-delivery` emits `order-delivered` or
`delivery-failed`, with a success scenario and an error scenario. Both
order-fulfillment examples are bumped to `3.4.0`.

**Not enforced here:** that `ingressId` names an ingress, and that `deliveryIdField`
names a field of every command the ingress feeds. Reference checks for a lint.

## 3.3.0

**Additive (non-breaking):**
- `stateChange` and `automation` slices gain an optional `outcomes`: a list of
  alternatives, each a list of events emitted together. `[["a", "b"], ["c"]]` means
  "A and B together, or C alone". An empty alternative, `[]`, is a success that emits
  nothing, such as an idempotent repeat.
- `eventIds` (and `resultEventIds`) keep their meaning: every event the command can
  emit. Without `outcomes`, whether those events come together or as alternatives is
  unspecified, as before.
- Raised by `project/container-paas` (decision 0008, bounded state machines): a
  transition emitting `A and B` looked identical to one emitting `A or B`, so the
  difference could only be written in prose.

**Examples:** `auto-ship-slice` declares `[["order-shipped"], []]`: it ships the order,
or emits nothing when the order has already shipped. Both order-fulfillment examples
are bumped to `3.3.0`.

**Not enforced here:** that every id in `outcomes` is in `eventIds` (or
`resultEventIds`), and that together they cover it. Reference checks for a lint.

## 3.2.0

**Additive (non-breaking):**
- `readModel` gains `selfAccess`: `{ "subjectField", "via"? }`. A caller holding the
  read model's `requiredRole` sees every row, as before. Any other signed-in caller
  sees only the rows whose `subjectField` equals their own subject id. Any read model
  can declare it, not only ones holding PII ("my orders" needs it as much as "my
  profile").
  - Without `via`, the caller's id (as the host authenticates it) is their subject id.
  - With `via` (`readModelId`, `keyField`, `ownerField`, the same shape as
    `command.requiredOwnership.via`), the caller's subject ids are the `keyField`
    values of the rows in `readModelId` whose `ownerField` equals the caller's id.
    It exists because a subject id need not be the login id: erasure is final per
    subject, so a person who comes back is a new subject.
  - With `selfAccess` and no `requiredRole`, every caller is limited to their own rows.
  - It narrows `scopes` and `filters`, never widens them.
- New top-level `dataSubjects`, holding `erasure`: `{ "self"?, "roles"?, "via"? }`,
  which says who may call the runtime's built-in `EraseSubject`. `self: true` lets a
  subject erase themselves (resolved as above, `via` included); `roles` lists roles
  that may erase anyone. At least one of `self: true` or `roles` is required, and
  `via` is only accepted with `self: true`. Without the declaration nothing changes:
  authorizing `EraseSubject` stays the host's job. Re-authentication and confirmation
  are always the host's. This is a declaration, not a modelled data-subject
  lifecycle (that stays a runtime concern).
  **Corrected in 3.7.1:** the declaration authorises the *request* that starts
  erasure (`RequestErasure` where the runtime governs erasure), never `EraseSubject`.
- Split documents carry `dataSubjects` inline in the manifest, like `swimlanes`.
  `manifest.schema.json`, `split.js` and `join.js` are updated to match.
- Raised by `platform/eventmodeling-codegen`: a person reading or erasing their own
  data (GDPR access, portability and erasure) had no way to be declared. The
  command side has had `requiredOwnership` since 2.5.0; the read side had only
  "this role sees every row".

**Internal, no document-visible change:** `command.requiredOwnership.via`'s shape
moves to a shared `$def`, `ownershipVia`, now also used by `selfAccess.via` and
`erasure.via`.

**Examples:** `order-summary` gains `customerId`, `requiredRole: "support"` and
`selfAccess` on `customerId`, and the document declares
`dataSubjects.erasure` with `self: true` and `roles: ["support"]`. Both
order-fulfillment examples are bumped to `3.2.0`.

**Not enforced here:** that `subjectField` names a top-level field of the same read
model, and that `via.readModelId` names an existing read model with `keyField` and
`ownerField` among its fields. These are reference checks for a generator or lint,
like `piiSubject`.

## 3.1.1

**Clarification (no schema shape change):**
- The `match` normalizers are now defined exactly, with test vectors, in
  `docs/design-notes.md` (v3.1.1). A hashed index only matches when every
  implementation normalizes byte-identically, and v3.1.0's definitions were loose
  enough for two implementations to disagree silently.
- `caseFold`/`email`: NFKC, then invariant one-to-one lowercase per code point (not
  full case folding, so `ß` stays `ß`; `U+0130` is left unchanged), then trim.
- `personName`: `caseFold`, then NFD, drop non-spacing marks, collapse whitespace,
  then NFC.
- `phone`: keep a leading ASCII `+`, keep ASCII digits, drop everything else. No NFKC
  and no country inference, so a local and an international form of one number don't
  match. v3.1.0's "E.164 digits" wasn't implementable without a country.
- Documents need no change: anything valid under 3.1.0 is valid under 3.1.1. The
  `eventModelingSchemaVersion` default and both order-fulfillment examples are bumped.

## 3.1.0

**Additive (non-breaking):**
- `readModel.filters` gains a second kind, `match`: a field that can be searched by
  a query param. It takes a required `mode` (`exact`, `prefix`, `contains`) and
  optionally `normalize` (`none`, `caseFold`, `email`, `phone`, `personName`;
  default `caseFold`) and, for `prefix` only, `minPrefixLength` (at least 2;
  default 3).
- A document declares *how a field is matched*, never how it is stored. For a
  `pii: true` field, generators choose the least-revealing index that satisfies the
  mode: a keyed hash for `exact` and `prefix`, and normalized plaintext only for
  `contains`. They delete a data subject's index entries when that subject is
  erased. See `docs/design-notes.md`, v3.1.0.
- `phonetic` matching is deliberately **not** in this version. It needs one
  algorithm pinned identically across implementations, and will come in a later
  minor version once a real document needs it.
- Raised by `platform/eventmodeling-codegen`: a PII field is stored encrypted, and
  the same value encrypts differently every time, so before this change it could
  not be searched at all, and nothing in a document could say it should be.

**Examples:** `pending-shipments` gains `orderId`, `customerId` and a PII
`customerEmail`, plus an `exact` email match filter. Both order-fulfillment
examples are bumped to `3.1.0`.

**Not enforced here:** that `field` names a field of the same read model, and that
`prefix`/`contains` target a `string` field. Both are reference checks for a
generator/lint, like `piiSubject`.

## 3.0.0

**Breaking:**
- `field` gains `piiSubject`: the name of a sibling field (same `fields`/`subfields`
  array) whose value is the id of the data subject a PII value belongs to. The value
  is encrypted under that subject's key, so destroying the key (erasure by
  crypto-shredding) makes it unreadable. **Required whenever `pii: true`**, and
  `piiSubject` without `pii: true` is rejected. A 2.x document with a bare
  `pii: true` no longer validates, hence the major bump.
- `piiSubject` names a field only. There are deliberately no sentinels (no
  "aggregate id", no "acting user"). A subject is always a value you can see in the
  payload. See `docs/design-notes.md`, v3.0.0.
- Raised by `platform/eventmodeling-codegen`, which is building crypto-shredding on
  top of `pii`. Until now `pii` was a bare flag, so nothing said *whose* key to use,
  and no default is safe: the aggregate id is wrong whenever a stream holds someone
  else's data.

**Migrating a 2.x document:** for every `pii: true` field, add `piiSubject` naming the
field that holds the owner's id. If the element doesn't carry that id yet, add it as
a field. The worked example did exactly this: `order-placed` gains `customerId`
(which its own scenario already emitted) and `customerEmail` names it.

**Not enforced here:** that `piiSubject` names a field that actually exists on the
same element, and that the named field isn't itself `pii` (a subject id has to stay
readable to find the key). Both are reference checks JSON Schema can't express, so
they belong to a generator/lint, which should error rather than guess.

## 2.7.0

**Additive (non-breaking):**
- `readModel` gains `requiredRole` — the read-side mirror of `command.requiredRole`
  (2.5.0): the actor's own role must be a member of the declared role(s) to query
  this read model at all. Raised by `project/timesheets`
  (`build-plan/00-decisions-and-blockers.md` D12): every generated read-model query
  route required only *an* authenticated actor, never a role, so any signed-in user
  could read any tenant's full data (confirmed in a real browser — a Staff-role
  account reading the full staff roster including hourly costs).
- **Internal rename, no document-visible change**: the `$def` backing `requiredRole`
  (`command.requiredRole`, `commandFieldGatedRole.requiredRole`, and now
  `readModel.requiredRole`) is renamed from `commandRole` to `roleRequirement` — it
  was never actually command-specific (already shared by `commandFieldGatedRole`
  before this release), and reads oddly once a `readModel` property references it
  too. `$def` names aren't part of a document's own vocabulary — nothing an author
  writes changes.

A 2.6.0 document validates unchanged against 2.7.0. Consuming `readModel.requiredRole`
(a generated `ReadModelAuthorization` check in every generated query route,
structurally parallel to `CommandAuthorization`) is `platform/eventmodeling-codegen`'s
job, not this schema's — same split every other read/write-side capability here has
kept (`filters`, `asOf`, `command.requiredRole` itself).

## 2.6.0

**Additive (non-breaking):**
- `readModelQuery` (a scenario's `when`/`then` for a `stateView` slice) gains an
  optional `asOf` (ISO 8601 date, `YYYY-MM-DD`), sibling to `queryParams`. Meaningful
  only alongside a `filters`-declared (2.4.0) `dateRangePreset` param: when present, a
  verify runner should resolve `last7Days`/`lastCalendarMonth` against `asOf` instead of
  the live clock, letting a scenario pin "today" once instead of drifting out of its own
  window on a rolling cadence. Absent, behavior is unchanged (today's live-clock
  resolution).
- Consuming `asOf` (stubbing the verify-time clock) is a generator's job
  (`platform/eventmodeling-codegen`), not this schema's — same split as `filters`
  itself.

A 2.5.0 document validates unchanged against 2.6.0.

## 2.5.0

**Additive (non-breaking):**
- `command` gains four optional authorization declarations, from the signed-off
  `platform/command-authorization` design proposal:
  - `requiredRole` (new `$def` `commandRole`: a role id, or a non-empty array of
    role ids) — the actor's own role must match one of them.
  - `fieldGatedRole` (new `$def` `commandFieldGatedRole`: `{ field, value,
    requiredRole }`) — `requiredRole` applies only when the command payload's
    `field` equals `value`.
  - `requiredOwnership` (new `$def` `commandOwnership`: `{ bypassRoles?, via:
    { readModelId, keyField, ownerField } }`) — the actor's own id must equal
    the value of `ownerField` on the read-model row `via` resolves (keyed by
    the command's own target), unless the actor's role is in `bypassRoles`.
  - `scope` (new `$def` `commandScope`: `{ bypassRoles?, resolveVia: {
    readModelId, keyField, selectField }, memberOfVia: { readModelId,
    matchField } }`) — resolves a value from the target's own read-model row,
    then the actor's own id must be a member of the set `memberOfVia`
    resolves for that value, unless the actor's role is in `bypassRoles`.

  `requiredOwnership` and `scope` are kept as two separate declarations rather
  than merged into one — see `docs/design-notes.md` for why (a genuine second
  resolution hop distinguishes them: equality-against-actor vs.
  set-membership).

See `docs/design-notes.md` ("v2.5.0: command authorization") for the full
design rationale, including why `requiredOwnership`'s resolution needed a
read-model `via` lookup rather than a bare field name, and what this addition
deliberately leaves to the host layer (how an actor's role claim itself gets
populated — including for an "administrator" role that lives outside any
`staff`-shaped aggregate).

A 2.4.0 document validates unchanged against 2.5.0.

## 2.4.0

**Additive (non-breaking):**
- `readModel` gains an optional `filters` (new `$def` `readModelFilter`, array of
  `{ param, field, kind, presets }`): declares a single-field WHERE-range query filter
  against one of the read model's own columns, with named presets (`last7Days`/
  `lastCalendarMonth`/`custom`, new `$def` `dateRangePreset`) rather than a raw date
  range. `kind` is currently always `"dateRange"` (new `$def` `filterKind`), shaped as a
  discriminator for a possible future second kind.

See `docs/design-notes.md` ("v2.4.0: `readModel.filters`...") for why presets are a
closed enum, the documented (not schema-enforced) runtime `queryParams` value
convention, and why this deliberately does not solve `staffTotals`-style cross-row
correlation.

A 2.3.0 document validates unchanged against 2.4.0.

## 2.3.0

**Additive (non-breaking):**
- `fieldDerivationKind` gains `"groupBy"` — a nested/grouped-rollup fold, computing a
  `cardinality: "list"` field's `subfields` as one row per distinct value of a source
  event payload field (`groupByField`), with each subfield computed within its group by
  an ordinary nested `sum`/`count`/`toggle` `derivation`. `field` gains a matching
  cross-property constraint: `derivation.kind: "groupBy"` requires `cardinality: "list"`
  and a non-empty `subfields`.

See `docs/design-notes.md` ("v2.3.0: grouped-rollup derivation (`groupBy`)") for why
this reuses `field`'s existing recursive `subfields` shape rather than adding a new
`$def`, and for what it deliberately does not solve (row-scoping by date range,
value-filtering contributing events — see the still-open `dateRange` capability).

A 2.2.0 document validates unchanged against 2.3.0.

## 2.2.0

**Additive (non-breaking):**
- `field` gains an optional `derivation` (new `$def` `fieldDerivation`): computes a
  read-model field as a fold over named events instead of copying a same-named
  payload key. Three kinds — `toggle` (`onEventIds`/`offEventIds`/`initial`),
  `count` (`incrementOnEventIds`/`decrementOnEventIds`/`rowKeyField`), `sum`
  (`addOnEventIds`/`subtractOnEventIds`/`amountField`/`rowKeyField`).
- `event` gains an optional `endsStream` (boolean, default `false`) — marks an
  event that resets a stream's synthesized existence to `false`, the write-side
  counterpart of a `toggle`'s "off" event.
- `readModel` gains an optional `scopes` (new `$def` `readModelScope`, array of
  `{ param, via: { readModelId, matchParamTo, selectField, filterLocalField } }`):
  declares that a stateView query param resolves through a different read model
  rather than naming one of this read model's own columns.

See `docs/design-notes.md` ("v2.2.0: derived read-model fields, stream-ending
events, scoped queries") for the four recurring codegen gaps this closes and why
each shape landed where it did.

A 2.1.0 document validates unchanged against 2.2.0.

## 2.1.0

**Additive (non-breaking):**
- `sliceStatus` gains `"accepted"` — the notation-layer value for a slice that has
  been reviewed and signed off for build, distinct from `"review"` (under review,
  not yet agreed) and `"done"` (built). Authoring workflows that move a slice
  `planned → accepted` on committing to build it were producing documents no
  `sliceStatus` value fit, which surfaced as a confusing cascade: an out-of-enum
  `status` fails the `sliceBase` `$ref` inside `slice`'s `allOf`, so its evaluated
  properties are dropped and the sibling `unevaluatedProperties: false` then flags
  `id`/`name`/`swimlaneId`/`chapterId`/`businessCapability`/`status` on every
  slice. The `slice` shape itself was never at fault. See design-notes.md
  ("Slice status has a conventional default...").

A 2.0.0 document validates unchanged against 2.1.0.

## 2.0.0

**Breaking:**
- Removed the `translation` slice pattern and scenario kind entirely
  (`Event(s) → Read Model → Event(s)`, no command). It never had a valid
  Given/When/Then shape once checked against primary EventModeling sources
  (Adam Dymitruk's own article and a canonical worked blueprint) rather than a
  secondary cheat sheet — a Read Model can be consulted for context but never
  originates an Event; only a Command can. A v1 document using
  `pattern: "translation"` will not validate against 2.0.0. See design-notes.md
  ("v2: `translation` removed...").

**Additive (non-breaking):**
- Optional typed `field` system (name/type/optional/cardinality/`idAttribute`/`pii`/
  recursive `subfields`) on `event`, `command`, and `readModel` definitions.
- `automation`'s `readModelId` changed from required to optional, supporting
  stateless boundary-crossing automations ("Bridge") that go straight from event
  to command with no persisted state.
- Optional `aggregate` (string, type-level) tag on `event` and `command`
  definitions.
- A multi-file composition layer: `schema/manifest.schema.json` plus
  `schema/scripts/{split,join,roundtrip-check}.js`, letting a document be split
  into a manifest + one file per registry/slice and joined back losslessly. No
  change to the core document schema's shape.

A v1.0.0 document with no `translation` slices, no `fields`, no `aggregate` tags
validates unchanged against 2.0.0.

## 1.0.0

Initial draft: swimlanes, the 5 elements (Event/Command/ReadModel/Screen/
Automation), 4 slice patterns (State Change/State View/Automation/Translation),
scenarios, and the optional notation layer (hotspots, chapters, actor lanes,
slice status).
