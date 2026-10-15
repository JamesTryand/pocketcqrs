# Personal data

A field marked `pii` in an EventModeling document is **crypto-shredded**. Every value is
encrypted under the key of the data subject it belongs to before it reaches the append-only log.
Erasing the subject destroys that key, so every copy becomes unreadable: in the log, in read
models, in backups. The log itself is never edited.

The keys live in the key-management facade (`journal.bureau.tryand.uk/kms`). pocketcqrs only ever
asks it to encrypt, decrypt, hash and destroy, and a key never leaves it. This is the same design
dotnetcqrs uses, with the same wire format, so the two stacks agree on every stored value.

**Go target only, for now.** `schema import --lang go` generates all of this. `--lang js` refuses
a document with `pii` fields rather than writing them as plain columns. JS support is planned as
its own step.

## Declaring it

```json
{ "name": "customerId", "type": "string" },
{ "name": "email", "type": "string", "pii": true, "piiSubject": "customerId" }
```

`piiSubject` (schema 3.0.0) names the sibling field whose value is the subject's id. The importer
refuses anything it can't encrypt correctly:

- a `pii` field without `piiSubject`, or `piiSubject` without `pii`;
- a subject that isn't a sibling, is itself `pii`, or isn't text;
- a read-model column whose `pii` flag disagrees with the same field on the events that build it;
- `pii` on anything compared in SQL: a row key, a scope, a `dateRange` filter, or a derivation's
  key or amount;
- `pii` inside `subfields` (top-level fields only, for now).

A subject named like personal data (`email`, `phone`, ...) gets a warning. **Erasure is terminal:
a person who returns is a new subject with a new id.** Use an opaque id, never the email.

## What the generated code does

| Step | What happens |
|---|---|
| Command arrives at the gateway | A `$pii` object anywhere in the payload is a `400`: callers send plaintext, the platform encrypts. |
| Batching writer queues it | The command's pii fields are **sealed** (encrypted) before the row is written, so `commandqueue.db` never holds plaintext. |
| `Decide` | Sees plaintext when it reads a value: the shell decrypts on demand and runs `Decide` again, which is safe because it is pure. A value only copied forward is never decrypted. |
| Append | Fresh values are encrypted under their subject's key. A value can't be stored under a different subject than it was sealed for. Without the generated protector, the command fails rather than store plaintext. |
| Projection | Stores the `{"$pii":{"s":...,"c":...}}` envelope text as it is. |
| Read route | `GET /api/cqrs/rm/{collection}` decrypts each page with one batch call per subject. |

Status codes: an erased subject is `410`; the key service being unavailable is `503` (retry);
a missing key is a `500` wiring fault.

## Reading it back

Each read model gets a generated route spec (`<collection>_query.go`) served by package
`queryroute`.

- **Who sees what.** A caller with the read model's `requiredRole` sees every row; superusers
  see everything. With the optional `SubjectOf` hook, a person without the role sees **only their
  own rows**. That is the "show me my email" case, and it is valid: GDPR Art. 15 gives people the
  right to their own data. Self-access works only when every pii column of the read model shares
  one subject column. **A read model holding personal data with no `requiredRole` is closed by
  default** (superusers, plus self-access), and the importer warns about it.
- **Erased values** read `{"$redacted":true}`, distinct from `null`.
- **A pii column can't be filtered or sorted on** (`400`): it is ciphertext.
- **Registering the route locks the collection.** Its List and View rules are set to
  superuser-only, so PocketBase's own record API (list, view, realtime, expand) refuses everyone
  else. The route is the only read path.

## Searching it

A `match` filter (schema 3.1.0) makes a field searchable by a query param. Indexes live in
`search.db`, beside the other data files:

| Field | `exact` | `prefix` | `contains` |
|---|---|---|---|
| personal data | blind index: the facade's HMAC of the normalized value | HMAC of every prefix of `minPrefixLength`+ characters | normalized **plaintext** (warned at import) |
| plain | normalized value | normalized value | normalized value |

- **Normalization** follows schema 3.1.1's pinned rules byte for byte (package `match`), so a
  hashed index matches what dotnetcqrs writes.
- **Exclude `search.db` from backups.** It is rebuilt from the log, and its checkpoints live inside
  it, so a lost or unrestored file rebuilds from zero rather than staying partly empty.
- **Erasure.** A subject's rows are deleted when `SubjectErased` arrives, and again at startup from
  the facade's erasure ledger (`PurgeErased`). A value whose key is already destroyed is never
  indexed.
- **Key rotation** is noticed at startup. The index rebuilds at the new key version while searches
  stay on the old one, then switches over.
- **`prefix` over personal data** is only as private as access to the facade's `hmac` endpoint:
  anyone who can call it can confirm a guessed first few characters and extend the guess.
- **Limits.** A search matching more than 1000 rows is a `400`: narrow it.

## Wiring a host

`schema import --lang go` prints the lines. In outline:

```go
piiDeps, err := pii.Wire(ctx, pii.WireOptions{Registry: registry, Engine: engine,
	Checkpoints: store, Loader: store, Writer: true}) // Writer false on a secondary
decider.RegisterProtected(registry, "order", order.Order(), order.NewProtector(piiDeps))

searchStore, err := searchindex.Open(filepath.Join(dataDir, "search.db"))
_, err = searchStore.PurgeErased(ctx, piiDeps.KMS)
idx, err := searchindex.Register(ctx, engine, searchStore, order.PendingShipmentsSearch, &piiDeps, indexKey)

readCfg := queryroute.Config{PII: &piiDeps, ResolveOwnRole: ownRole, SubjectOf: nil,
	Indexes: map[string]*searchindex.Index{"pendingShipments": idx}}
err = queryroute.Register(se, order.PendingShipmentsQuery, readCfg)
```

| Setting | Meaning |
|---|---|
| `KMS_FACADE_URL` | The facade's base URL. **Required:** `pii.Wire` refuses to start without it, and there is no in-memory fallback. |
| `KMS_INDEX_KEY` | The facade HMAC key this host's blind indexes use (one per host). |

`pii.Wire` registers the built-in `dataSubject` aggregate. It also runs the key destroyer on the
writer and a cache evictor on every node, seeded from the destroyer's position. Decrypted values
are cached per process, bounded and never persisted, so warm reads survive a key-service outage.

## Erasing someone

```
POST /api/cqrs/dataSubject/{subjectId}/EraseSubject
```

The command appends `SubjectErased` and is idempotent. The key destroyer then destroys the key,
caches drop the subject, and the search indexes delete their rows. From then on, reads return
`{"$redacted":true}` and new personal data for that id is refused with `410`. **Erasure has a
bounded lag** (it is a projection of the event), not instant effect.

**Erasure is closed until you decide who may do it.** With no `Authorize` hook on the gateway,
every command is allowed, so the gateway refuses every `dataSubject` command with `403` and records
nothing. Wire `gateway.Config.Authorize` with a policy saying who may erase whom (an administrator
role, say). Once a hook is wired, `dataSubject` commands go through it like any other command. A
document can't declare this yet: schema 3.2.0's `dataSubjects` is refused on import (it authorises a
*request* for erasure, which pocketcqrs doesn't model yet).
