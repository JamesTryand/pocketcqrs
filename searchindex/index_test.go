package searchindex_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/kms"
	"github.com/jamestryand/pocketcqrs/match"
	"github.com/jamestryand/pocketcqrs/pii"
	"github.com/jamestryand/pocketcqrs/searchindex"
)

var (
	emailExact  = searchindex.Field{Param: "email", Field: "email", Mode: match.Exact, Normalize: match.Email, PII: true}
	emailPrefix = searchindex.Field{Param: "emailStarts", Field: "email", Mode: match.Prefix, MinPrefix: 3, PII: true}
	nameHas     = searchindex.Field{Param: "q", Field: "name", Mode: match.Contains, Normalize: match.PersonName, PII: true}
	cityExact   = searchindex.Field{Param: "city", Field: "city", Mode: match.Exact}
	cityPrefix  = searchindex.Field{Param: "cityStarts", Field: "city", Mode: match.Prefix}
)

var spec = searchindex.Spec{
	Collection: "customers",
	Events:     []string{"CustomerRegistered"},
	Fields:     []searchindex.Field{emailExact, emailPrefix, nameHas, cityExact, cityPrefix},
}

type fx struct {
	ctx    context.Context
	events *events.Store
	reg    *decider.Registry
	kms    *kms.Memory
	deps   *pii.Deps
	dir    string
}

func newFx(t *testing.T) *fx {
	t.Helper()
	dir := t.TempDir()
	es, err := events.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { es.Close() })
	reg := decider.NewRegistry(es)
	pii.RegisterDataSubjects(reg)
	m := kms.NewMemory()
	return &fx{ctx: context.Background(), events: es, reg: reg, kms: m, dir: dir,
		deps: &pii.Deps{KMS: m, Cache: pii.NewCache(0)}}
}

func (f *fx) envelope(t *testing.T, subject, v string) json.RawMessage {
	t.Helper()
	enc, err := pii.NewFresh(v).Encrypt(f.ctx, f.kms, subject, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(enc)
	return b
}

func (f *fx) register(t *testing.T, id, email, name, city string) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{
		"customerId": id, "email": f.envelope(t, id, email), "name": f.envelope(t, id, name), "city": city,
	})
	if _, err := f.events.Append(f.ctx, "customer", id, 0, []events.NewEvent{{Type: "CustomerRegistered", Data: data}}); err != nil {
		t.Fatal(err)
	}
}

func (f *fx) open(t *testing.T, name string) *searchindex.Store {
	t.Helper()
	s, err := searchindex.Open(filepath.Join(f.dir, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func (f *fx) index(t *testing.T, store *searchindex.Store) (*searchindex.Index, *consumers.Engine) {
	t.Helper()
	engine := consumers.NewEngine(f.events, nil)
	idx, err := searchindex.Register(f.ctx, engine, store, spec, f.deps, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	return idx, engine
}

func find(t *testing.T, idx *searchindex.Index, fl searchindex.Field, term string) []string {
	t.Helper()
	keys, err := idx.Search(context.Background(), fl, term)
	if err != nil {
		t.Fatalf("%s=%q: %v", fl.Param, term, err)
	}
	slices.Sort(keys)
	return keys
}

func TestEveryModeFindsTheRightRows(t *testing.T) {
	f := newFx(t)
	f.register(t, "c1", "Alice@Example.com", "José García", "London")
	f.register(t, "c2", "bob@example.com", "Bob Smith", "Londonderry")
	idx, _ := f.index(t, f.open(t, "search.db"))

	cases := []struct {
		field searchindex.Field
		term  string
		want  string
	}{
		{emailExact, " ALICE@example.COM ", "c1"}, // normalized both sides, hashed
		{emailExact, "alice@example", ""},         // exact means exact
		{emailPrefix, "ali", "c1"},                // a hashed prefix, one hash per search
		{emailPrefix, "alice@exa", "c1"},
		{nameHas, "garcia", "c1"},   // personName drops the accent
		{cityExact, "london", "c1"}, // plain, caseFold
		{cityPrefix, "LONDON", "c1,c2"},
	}
	for _, c := range cases {
		if got := strings.Join(find(t, idx, c.field, c.term), ","); got != c.want {
			t.Errorf("%s=%q: got %q want %q", c.field.Param, c.term, got, c.want)
		}
	}
	var bad *searchindex.BadTermError
	if _, err := idx.Search(f.ctx, emailPrefix, "al"); !errors.As(err, &bad) {
		t.Errorf("a prefix shorter than minPrefixLength is the caller's error: %v", err)
	}
	if _, err := idx.Search(f.ctx, cityExact, "   "); !errors.As(err, &bad) {
		t.Errorf("an empty term is the caller's error: %v", err)
	}
}

func TestHashedRowsHoldNoPlaintextAndContainsIsTheOneReadableForm(t *testing.T) {
	f := newFx(t)
	f.register(t, "c1", "alice@example.com", "Alice Smith", "London")
	f.index(t, f.open(t, "search.db"))
	raw, err := os.ReadFile(filepath.Join(f.dir, "search.db-wal"))
	if err != nil {
		raw, _ = os.ReadFile(filepath.Join(f.dir, "search.db"))
	}
	if strings.Contains(string(raw), "alice@example") {
		t.Fatal("a hashed (exact/prefix) index must never store the email itself")
	}
	if !strings.Contains(string(raw), "alice smith") {
		t.Fatal("contains is the one index that keeps normalized plaintext (why search.db is backup-excluded)")
	}
}

func TestErasureDeletesTheSubjectsRowsAndADestroyedKeyIsNeverIndexed(t *testing.T) {
	f := newFx(t)
	f.register(t, "c1", "alice@example.com", "Alice", "London")
	f.register(t, "c2", "bob@example.com", "Bob", "London")
	store := f.open(t, "search.db")
	idx, engine := f.index(t, store)
	if _, err := f.reg.Handle(f.ctx, pii.SubjectAggregate, "c1", decider.Command{Name: pii.EraseSubjectCommand}); err != nil {
		t.Fatal(err)
	}
	if err := engine.RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	for _, fl := range []searchindex.Field{emailExact, nameHas} {
		term := map[string]string{"email": "alice@example.com", "q": "alice"}[fl.Param]
		if got := find(t, idx, fl, term); len(got) != 0 {
			t.Errorf("%s: an erased subject must not be found: %v", fl.Param, got)
		}
	}
	if got := find(t, idx, nameHas, "bob"); len(got) != 1 {
		t.Error("only the erased subject's rows go")
	}

	// a key destroyed before indexing: nothing is indexed for it
	g := newFx(t)
	g.register(t, "c3", "carol@example.com", "Carol", "Leeds")
	_ = g.kms.DestroyKey(g.ctx, "c3")
	gidx, _ := g.index(t, g.open(t, "search.db"))
	if got := find(t, gidx, nameHas, "carol"); len(got) != 0 {
		t.Errorf("a destroyed key's value is never indexed: %v", got)
	}
	if got := find(t, gidx, cityExact, "leeds"); len(got) != 1 {
		t.Error("the record's plain fields are still indexed")
	}
}

func TestStartupPurgeFollowsTheErasureLedger(t *testing.T) {
	f := newFx(t)
	f.register(t, "c1", "alice@example.com", "Alice", "London")
	store := f.open(t, "search.db")
	idx, _ := f.index(t, store)
	_ = f.kms.DestroyKey(f.ctx, "c1") // erased while this node wasn't listening
	n, err := store.PurgeErased(f.ctx, f.kms)
	if err != nil || n == 0 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if got := find(t, idx, nameHas, "alice"); len(got) != 0 {
		t.Error("purged subject still found")
	}
	if n, err := store.PurgeErased(f.ctx, f.kms); err != nil || n != 0 {
		t.Fatalf("a second purge resumes from the cursor: %d %v", n, err)
	}
}

func TestLosingTheFileLosesItsCheckpointsSoItRebuilds(t *testing.T) {
	f := newFx(t)
	f.register(t, "c1", "alice@example.com", "Alice", "London")
	first := f.open(t, "search.db")
	f.index(t, first)
	if on, _ := first.SecureDelete(f.ctx); !on {
		t.Fatal("secure_delete must be on")
	}
	// a fresh file (deleted, or not restored from backup): positions start at zero
	idx, _ := f.index(t, f.open(t, "search-restored.db"))
	if got := find(t, idx, cityExact, "london"); len(got) != 1 {
		t.Fatal("a fresh index file must rebuild from the log, not stay empty")
	}
}

func TestRotationKeepsSearchingTheOldVersionThenSwitches(t *testing.T) {
	f := newFx(t)
	f.register(t, "c1", "alice@example.com", "Alice", "London")
	store := f.open(t, "search.db")
	f.index(t, store)
	if v, _ := store.ActiveVersion(searchindex.HashedIndexName("customers")); v != 1 {
		t.Fatalf("built at v1, got %d", v)
	}
	if err := f.kms.RotateIndexKey("shop"); err != nil {
		t.Fatal(err)
	}
	// restart: registration notices the rotation
	engine := consumers.NewEngine(f.events, nil)
	idx, err := searchindex.Register(f.ctx, engine, store, spec, f.deps, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := store.ActiveVersion(searchindex.HashedIndexName("customers")); v != 1 {
		t.Fatalf("searches stay on the old version until the rebuild catches up, got v%d", v)
	}
	if got := find(t, idx, emailExact, "alice@example.com"); len(got) != 1 {
		t.Fatal("still searchable during the rebuild")
	}
	if err := engine.RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := store.ActiveVersion(searchindex.HashedIndexName("customers")); v != 2 {
		t.Fatalf("caught up: switched to v2, got v%d", v)
	}
	if got := find(t, idx, emailExact, "alice@example.com"); len(got) != 1 {
		t.Fatal("found at the new version")
	}
	for _, n := range engine.Names() {
		if strings.HasSuffix(n, ":v1") {
			t.Fatalf("the old version's consumer must be gone: %v", engine.Names())
		}
	}
	// a restart after the switch is an ordinary start at v2
	engine2 := consumers.NewEngine(f.events, nil)
	if _, err := searchindex.Register(f.ctx, engine2, store, spec, f.deps, "shop"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(engine2.Names(), "customers:hashed:v2") || len(engine2.Names()) != 2 {
		t.Fatalf("plain + v2 only: %v", engine2.Names())
	}
}

func TestRegisterRefusesABadKeyName(t *testing.T) {
	f := newFx(t)
	engine := consumers.NewEngine(f.events, nil)
	if _, err := searchindex.Register(f.ctx, engine, f.open(t, "s.db"), spec, f.deps, "bad name"); err == nil {
		t.Fatal("an index key name the facade can't take must be refused")
	}
}
