package searchindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/kms"
	"github.com/jamestryand/pocketcqrs/match"
	"github.com/jamestryand/pocketcqrs/pii"
)

// EnvIndexKey names the facade HMAC key a host's hashed indexes use (one
// per host; defaults to something the host chooses, e.g. its name).
const EnvIndexKey = "KMS_INDEX_KEY"

var indexKeyName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Field is one match filter, as the document declares it.
type Field struct {
	Param     string
	Field     string
	Mode      string // match.Exact, match.Prefix, match.Contains
	Normalize string // "" means caseFold
	MinPrefix int    // prefix only, in code points
	PII       bool
}

// Hashed reports whether the field's index is a blind (HMAC) one:
// personal data searched exactly or by prefix.
func (f Field) Hashed() bool { return f.PII && f.Mode != match.Contains }

func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Table is the field's index table: one per (field, normalizer, mode).
func (f Field) Table(collection string) string {
	norm := f.Normalize
	if norm == "" {
		norm = match.CaseFold
	}
	return snake(collection) + "__" + snake(f.Field) + "_" + snake(norm) + "_" + f.Mode
}

// Spec is one read model's match filters and the events that feed it. Rows
// are keyed by the event's aggregate id, as the generated projection keys
// its rows.
type Spec struct {
	Collection string
	Events     []string
	Fields     []Field
}

func (s Spec) split() (plain, hashed []Field) {
	for _, f := range s.Fields {
		if f.Hashed() {
			hashed = append(hashed, f)
		} else {
			plain = append(plain, f)
		}
	}
	return plain, hashed
}

func tables(collection string, fs []Field) []string {
	var out []string
	for _, f := range fs {
		if t := f.Table(collection); !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// HashedIndexName is a read model's hashed index, for its key version.
func HashedIndexName(collection string) string { return collection + ":hashed" }

// value reads one field of an event: plaintext for a plain field; for a
// personal-data field, the revealed plaintext (or ok=false when the subject
// is erased or the value absent — never indexed). The subject comes back for
// erasure deletes.
func value(ctx context.Context, deps *pii.Deps, data map[string]json.RawMessage, f Field) (text, subject string, present, ok bool, err error) {
	raw, present := data[f.Field]
	if !present {
		return "", "", false, false, nil
	}
	if string(raw) == "null" {
		return "", "", true, false, nil
	}
	if !f.PII {
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", "", true, false, fmt.Errorf("searchindex: %s is not text: %w", f.Field, err)
		}
		return text, "", true, true, nil
	}
	if deps == nil {
		return "", "", true, false, errors.New("searchindex: personal-data field but no pii.Deps")
	}
	var v pii.Value[string]
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", "", true, false, err
	}
	if v.State() == pii.Fresh {
		return "", "", true, false, fmt.Errorf("searchindex: %s holds plaintext in the log; refusing to index it", f.Field)
	}
	buf := pii.NewBuffer(deps.KMS, deps.Cache)
	if err := pii.Reveal(buf, &v); err != nil {
		return "", "", true, false, err
	}
	if err := buf.Flush(ctx); err != nil {
		return "", "", true, false, err
	}
	if v.State() != pii.Known {
		return "", v.SubjectID(), true, false, nil // erased: never indexed
	}
	text, _ = v.Get()
	return text, v.SubjectID(), true, true, nil
}

func decodeData(ev events.Event) (map[string]json.RawMessage, error) {
	var data map[string]json.RawMessage
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		return nil, fmt.Errorf("searchindex: event %s data: %w", ev.Type, err)
	}
	return data, nil
}

func isErasure(ev events.Event) bool {
	return ev.Type == pii.SubjectErasedEvent && ev.Aggregate == pii.SubjectAggregate
}

// plainIndex keeps non-hashed fields: normalized text (non-pii fields, and
// pii contains).
type plainIndex struct {
	spec   Spec
	fields []Field
	store  *Store
	deps   *pii.Deps
}

func (p *plainIndex) Name() string                           { return p.spec.Collection + ":search" }
func (p *plainIndex) IsReadModel() bool                      { return true }
func (p *plainIndex) Checkpoints() consumers.CheckpointStore { return p.store }

func (p *plainIndex) Apply(ctx context.Context, ev events.Event) error {
	if isErasure(ev) {
		_, err := p.store.DeleteSubjects(ctx, []string{ev.AggregateID})
		return err
	}
	if !slices.Contains(p.spec.Events, ev.Type) {
		return nil
	}
	data, err := decodeData(ev)
	if err != nil {
		return err
	}
	for _, f := range p.fields {
		text, subject, present, ok, err := value(ctx, p.deps, data, f)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		var rows []row
		if ok {
			if norm, err := match.Normalize(f.Normalize, text); err != nil {
				return err
			} else if norm != "" {
				rows = []row{{term: norm, subject: subject}}
			}
		}
		if err := p.store.replace(ctx, f.Table(p.spec.Collection), ev.AggregateID, 0, rows); err != nil {
			return err
		}
	}
	return nil
}

// hashedIndex keeps blind (HMAC) rows at one key version.
type hashedIndex struct {
	spec    Spec
	fields  []Field
	store   *Store
	deps    *pii.Deps
	keyName string
	version int

	mu      sync.Mutex
	retired bool
	// onApplied, when set, runs after each applied event (rotation's
	// catch-up check).
	onApplied func(ctx context.Context, position int64) error
}

func (h *hashedIndex) Name() string {
	return fmt.Sprintf("%s:v%d", HashedIndexName(h.spec.Collection), h.version)
}
func (h *hashedIndex) IsReadModel() bool                      { return h.onApplied == nil } // a rebuild doesn't gate readiness
func (h *hashedIndex) Checkpoints() consumers.CheckpointStore { return h.store }

func (h *hashedIndex) retire() {
	h.mu.Lock()
	h.retired = true
	h.mu.Unlock()
}

func (h *hashedIndex) Apply(ctx context.Context, ev events.Event) error {
	h.mu.Lock()
	retired := h.retired
	h.mu.Unlock()
	if retired {
		return nil
	}
	if err := h.apply(ctx, ev); err != nil {
		return err
	}
	if h.onApplied != nil {
		return h.onApplied(ctx, ev.Position)
	}
	return nil
}

func (h *hashedIndex) apply(ctx context.Context, ev events.Event) error {
	if isErasure(ev) {
		_, err := h.store.DeleteSubjects(ctx, []string{ev.AggregateID})
		return err
	}
	if !slices.Contains(h.spec.Events, ev.Type) {
		return nil
	}
	data, err := decodeData(ev)
	if err != nil {
		return err
	}
	type pending struct {
		table   string
		subject string
		inputs  []string
		present bool
	}
	var work []pending
	var inputs [][]byte
	for _, f := range h.fields {
		text, subject, present, ok, err := value(ctx, h.deps, data, f)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		p := pending{table: f.Table(h.spec.Collection), subject: subject, present: true}
		if ok {
			norm, err := match.Normalize(f.Normalize, text)
			if err != nil {
				return err
			}
			if f.Mode == match.Prefix {
				p.inputs = match.Prefixes(norm, f.MinPrefix)
			} else if norm != "" {
				p.inputs = []string{norm}
			}
		}
		for _, in := range p.inputs {
			inputs = append(inputs, []byte(in))
		}
		work = append(work, p)
	}
	// every hash first (one hmac-batch per 1000 inputs, pinned to this
	// version), then the writes: a failed call changes nothing
	var hashes []string
	for start := 0; start < len(inputs); start += kms.MaxBatchItems {
		chunk := inputs[start:min(start+kms.MaxBatchItems, len(inputs))]
		items, err := h.deps.KMS.HMACBatch(ctx, h.keyName, chunk, h.version)
		if err != nil {
			return err
		}
		if len(items) != len(chunk) {
			return &kms.ProtocolError{Msg: "hmac-batch answered the wrong number of items"}
		}
		for _, it := range items {
			if !it.Succeeded() {
				return &kms.ProtocolError{Msg: "hmac-batch item error: " + it.Err}
			}
			hashes = append(hashes, string(it.Value))
		}
	}
	i := 0
	for _, p := range work {
		rows := make([]row, len(p.inputs))
		for j := range p.inputs {
			rows[j] = row{term: hashes[i], subject: p.subject}
			i++
		}
		if err := h.store.replace(ctx, p.table, ev.AggregateID, h.version, rows); err != nil {
			return err
		}
	}
	return nil
}

// Index is one registered read model's search.
type Index struct {
	Spec    Spec
	store   *Store
	deps    *pii.Deps
	keyName string
}

// Register sets up a read model's match indexes on engine. deps is required
// when any field is personal data, keyName when any is hashed (the facade
// HMAC key, ensured here). A hashed index's key version is checked once,
// here: unchanged → carry on; risen → rebuild at the new version beside the
// live one, switching over once it catches up; never built (or recorded
// above latest) → build from zero.
func Register(ctx context.Context, engine *consumers.Engine, store *Store, spec Spec, deps *pii.Deps, keyName string) (*Index, error) {
	plain, hashed := spec.split()
	for _, f := range spec.Fields {
		if f.PII && deps == nil {
			return nil, fmt.Errorf("searchindex %s: %s is personal data but no pii.Deps", spec.Collection, f.Field)
		}
	}
	for _, t := range tables(spec.Collection, spec.Fields) {
		if err := store.ensureTable(ctx, t); err != nil {
			return nil, err
		}
	}
	idx := &Index{Spec: spec, store: store, deps: deps, keyName: keyName}
	if len(plain) > 0 {
		engine.Register(&plainIndex{spec: spec, fields: plain, store: store, deps: deps})
	}
	if len(hashed) == 0 {
		return idx, nil
	}
	if !indexKeyName.MatchString(keyName) {
		return nil, fmt.Errorf("searchindex %s: index key name %q is not a valid facade key name", spec.Collection, keyName)
	}
	if err := deps.KMS.EnsureIndexKey(ctx, keyName); err != nil {
		return nil, err
	}
	latest, err := deps.KMS.IndexKeyVersion(ctx, keyName)
	if err != nil {
		return nil, err
	}
	name := HashedIndexName(spec.Collection)
	htables := tables(spec.Collection, hashed)
	recorded, built, err := store.indexVersion(ctx, name)
	if err != nil {
		return nil, err
	}
	mk := func(v int) *hashedIndex {
		return &hashedIndex{spec: spec, fields: hashed, store: store, deps: deps, keyName: keyName, version: v}
	}
	switch {
	case !built || recorded > latest:
		if err := store.truncate(ctx, htables); err != nil {
			return nil, err
		}
		if built {
			_ = store.deleteCheckpoint(ctx, mk(recorded).Name())
		}
		_ = store.deleteCheckpoint(ctx, mk(latest).Name())
		if err := store.setIndexVersion(ctx, name, latest); err != nil {
			return nil, err
		}
		store.setActive(name, latest)
		engine.Register(mk(latest))
	case recorded == latest:
		if err := store.deleteOtherVersions(ctx, htables, latest); err != nil {
			return nil, err
		}
		store.setActive(name, latest)
		engine.Register(mk(latest))
	default: // rotated: rebuild beside the live index, switch when caught up
		if err := store.deleteOtherVersions(ctx, htables, recorded, latest); err != nil {
			return nil, err
		}
		live, rebuild := mk(recorded), mk(latest)
		store.setActive(name, recorded)
		var once sync.Once
		switchOver := func(ctx context.Context) error {
			var err error
			once.Do(func() {
				if err = store.setIndexVersion(ctx, name, latest); err != nil {
					return
				}
				store.setActive(name, latest)
				live.retire()
				engine.Unregister(live.Name())
				_ = store.deleteCheckpoint(ctx, live.Name())
				err = store.deleteOtherVersions(ctx, htables, latest)
			})
			return err
		}
		livePos, _ := store.Checkpoint(ctx, live.Name())
		rebuildPos, _ := store.Checkpoint(ctx, rebuild.Name())
		if rebuildPos >= livePos { // already caught up (or nothing indexed yet)
			if err := switchOver(ctx); err != nil {
				return nil, err
			}
			engine.Register(rebuild)
			return idx, nil
		}
		rebuild.onApplied = func(ctx context.Context, position int64) error {
			lp, err := store.Checkpoint(ctx, live.Name())
			if err != nil {
				return err
			}
			if position >= lp {
				return switchOver(ctx)
			}
			return nil
		}
		engine.Register(live) // first: the engine runs consumers in registration order
		engine.Register(rebuild)
	}
	return idx, nil
}

// ErrTooBroad: a search matched more rows than a page of results can be
// filtered by. Narrow the term.
var ErrTooBroad = errors.New("searchindex: the search matches too many rows; narrow it")

// MaxMatches bounds the row keys one search may return.
const MaxMatches = 1000

// Search returns the row keys matching term for one filter: the term is
// normalized by the filter's rule; a hashed filter hashes it at the active
// key version (one hmac call); contains and plain prefix use LIKE. An empty
// or too-short term is the caller's error (BadTermError).
func (idx *Index) Search(ctx context.Context, f Field, term string) ([]string, error) {
	norm, err := match.Normalize(f.Normalize, term)
	if err != nil {
		return nil, err
	}
	if norm == "" {
		return nil, &BadTermError{fmt.Sprintf("'%s' needs a non-empty search term", f.Param)}
	}
	if f.Mode == match.Prefix && f.MinPrefix > 0 && match.CodePointLength(norm) < f.MinPrefix {
		return nil, &BadTermError{fmt.Sprintf("'%s' needs at least %d characters", f.Param, f.MinPrefix)}
	}
	table := f.Table(idx.Spec.Collection)
	var keys []string
	if f.Hashed() {
		v, ok := idx.store.ActiveVersion(HashedIndexName(idx.Spec.Collection))
		if !ok {
			return nil, fmt.Errorf("searchindex: %s is not registered", idx.Spec.Collection)
		}
		h, err := idx.deps.KMS.HMAC(ctx, idx.keyName, []byte(norm), v)
		if err != nil {
			return nil, err
		}
		keys, err = idx.store.Lookup(ctx, table, v, "=", h, MaxMatches)
		if err != nil {
			return nil, err
		}
	} else {
		op, pat := "=", norm
		switch f.Mode {
		case match.Prefix:
			op, pat = "LIKE", match.EscapeLike(norm)+"%"
		case match.Contains:
			op, pat = "LIKE", "%"+match.EscapeLike(norm)+"%"
		}
		if keys, err = idx.store.Lookup(ctx, table, 0, op, pat, MaxMatches); err != nil {
			return nil, err
		}
	}
	if len(keys) > MaxMatches {
		return nil, ErrTooBroad
	}
	return keys, nil
}

// BadTermError is a search term the filter can't use (a 400).
type BadTermError struct{ Msg string }

func (e *BadTermError) Error() string { return e.Msg }
