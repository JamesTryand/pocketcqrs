package pii_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/batching"
	"github.com/jamestryand/pocketcqrs/commandqueue"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/kms"
	"github.com/jamestryand/pocketcqrs/pii"

	_ "modernc.org/sqlite"
)

// customer is hand-written in exactly the shape the Go generator emits for
// an aggregate with a pii field (email, subject customerId), so the decider
// shell is proven before any generator exists.

type customerState struct {
	Registered bool
	CustomerID string
	Email      pii.Value[string]
}

type registered struct {
	CustomerID string            `json:"customerId"`
	Email      pii.Value[string] `json:"email"`
}

type customerIn struct {
	CustomerID string            `json:"customerId"`
	Email      pii.Value[string] `json:"email"`
}

func customer() *decider.Decider[customerState] {
	return &decider.Decider[customerState]{
		InitialState: func() customerState { return customerState{} },
		Decide: func(cmd decider.Command, s customerState) ([]events.NewEvent, error) {
			switch cmd.Name {
			case "Register": // reads the command's email (validation)
				var in customerIn
				if err := json.Unmarshal(cmd.Payload, &in); err != nil {
					return nil, err
				}
				if err := pii.ResolveIn(cmd, &in.Email); err != nil {
					return nil, err
				}
				email, err := in.Email.Get()
				if err != nil {
					return nil, err
				}
				if !strings.Contains(email, "@") {
					return nil, errors.New("email is not an address")
				}
				return []events.NewEvent{{Type: "Registered", Payload: &registered{in.CustomerID, in.Email}}}, nil
			case "Import": // copies the email without reading it
				var in customerIn
				if err := json.Unmarshal(cmd.Payload, &in); err != nil {
					return nil, err
				}
				return []events.NewEvent{{Type: "Registered", Payload: &registered{in.CustomerID, in.Email}}}, nil
			case "Reconfirm": // copies stored pii forward, never reads it
				return []events.NewEvent{{Type: "Registered", Payload: &registered{s.CustomerID, s.Email}}}, nil
			case "CheckDomain": // reads stored pii
				email, err := s.Email.Get()
				if err != nil {
					return nil, err
				}
				if !strings.HasSuffix(email, "@example.com") {
					return nil, errors.New("wrong domain")
				}
				return nil, nil
			}
			return nil, errors.New("unknown command: " + cmd.Name)
		},
		Evolve: func(s customerState, ev events.Event) (customerState, error) {
			var p registered
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				return s, err
			}
			return customerState{Registered: true, CustomerID: p.CustomerID, Email: p.Email}, nil
		},
	}
}

type customerProtector struct{ d pii.Deps }

func (p customerProtector) Reveal(ctx context.Context, s customerState, cmd decider.Command) (customerState, decider.Command, error) {
	buf := pii.NewBuffer(p.d.KMS, p.d.Cache)
	if err := pii.Reveal(buf, &s.Email); err != nil {
		return s, cmd, err
	}
	cr := pii.QueueCommand(buf, cmd)
	if err := buf.Flush(ctx); err != nil {
		return s, cmd, err
	}
	return s, cr.Apply(cmd), nil
}

func (p customerProtector) Protect(ctx context.Context, _ string, evs []events.NewEvent) ([]events.NewEvent, error) {
	for i := range evs {
		if pl, ok := evs[i].Payload.(*registered); ok {
			var err error
			if pl.Email, err = pii.ProtectField(ctx, p.d, pl.Email, pl.CustomerID, "Registered.email", "customerId"); err != nil {
				return nil, err
			}
		}
	}
	return evs, nil
}

func (p customerProtector) Seal(ctx context.Context, cmd decider.Command) (json.RawMessage, error) {
	return pii.SealPayload(ctx, p.d, cmd.Payload, []pii.SealField{{Field: "email", Subject: "customerId"}})
}

type fixture struct {
	store *events.Store
	reg   *decider.Registry
	kms   *kms.Memory
	cache *pii.Cache
	deps  pii.Deps
}

func setup(t *testing.T, protected bool) *fixture {
	t.Helper()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := &fixture{store: store, reg: decider.NewRegistry(store), kms: kms.NewMemory(), cache: pii.NewCache(0)}
	f.deps = pii.Deps{KMS: f.kms, Cache: f.cache, Subjects: pii.StoreSubjectStatus{Loader: store}}
	if protected {
		decider.RegisterProtected(f.reg, "customer", customer(), customerProtector{f.deps})
	} else {
		decider.Register(f.reg, "customer", customer())
	}
	pii.RegisterDataSubjects(f.reg)
	return f
}

func (f *fixture) handle(t *testing.T, id, name, payload string) ([]events.Event, error) {
	t.Helper()
	return f.reg.Handle(context.Background(), "customer", id, decider.Command{Name: name, Payload: json.RawMessage(payload)})
}

const alice = `{"customerId":"c1","email":"alice@example.com"}`

func TestEncryptedBeforeAppendAndDecideSawPlaintext(t *testing.T) {
	f := setup(t, true)
	evs, err := f.handle(t, "c1", "Register", alice)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(evs[0].Data), "alice") || !strings.Contains(string(evs[0].Data), `"$pii"`) {
		t.Fatalf("the log must hold only the envelope: %s", evs[0].Data)
	}
	if _, err := f.handle(t, "c2", "Register", `{"customerId":"c2","email":"not-an-address"}`); !decider.IsRejection(err) {
		t.Fatalf("decide saw the plaintext and rejected it: %v", err)
	}
}

func TestNoRevealWhenDecideDoesNotReadStoredPII(t *testing.T) {
	f := setup(t, true)
	if _, err := f.handle(t, "c1", "Register", alice); err != nil {
		t.Fatal(err)
	}
	f.cache.MarkErased("nobody") // keep the cache, but make sure it isn't what saves the call
	before, _, _ := f.kms.Calls()
	evs, err := f.handle(t, "c1", "Reconfirm", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	enc, dec, _ := f.kms.Calls()
	if dec != 0 || enc != before {
		t.Fatalf("copying stored pii forward must cost nothing: %d decrypts, %d encrypts", dec, enc-before)
	}
	if !strings.Contains(string(evs[0].Data), `"$pii"`) {
		t.Fatal("still an envelope")
	}
}

func TestReadingStoredPIIRevealsOnceAndDecidesAgain(t *testing.T) {
	f := setup(t, true)
	if _, err := f.handle(t, "c1", "Register", alice); err != nil {
		t.Fatal(err)
	}
	cold := pii.NewCache(0) // a fresh process: nothing cached
	decider.RegisterProtected(f.reg, "customer", customer(), customerProtector{pii.Deps{KMS: f.kms, Cache: cold}})
	if _, err := f.handle(t, "c1", "CheckDomain", `{}`); err != nil {
		t.Fatalf("one reveal, then the decision: %v", err)
	}
	if _, dec, _ := f.kms.Calls(); dec != 1 {
		t.Fatalf("want exactly one decrypt-batch, got %d", dec)
	}
	if _, err := f.handle(t, "c1", "CheckDomain", `{}`); err != nil {
		t.Fatal(err)
	}
	if _, dec, _ := f.kms.Calls(); dec != 1 {
		t.Fatal("the second read must be a cache hit")
	}
}

func TestWithoutAProtectorItFailsClosedAndIsNotARejection(t *testing.T) {
	f := setup(t, false)
	_, err := f.handle(t, "c1", "Register", alice)
	if err == nil || decider.IsRejection(err) || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("plaintext must never be appended, and it isn't the command's fault: %v", err)
	}
	evs, _ := f.store.LoadStream(context.Background(), "customer", "c1")
	if len(evs) != 0 {
		t.Fatal("nothing may be written")
	}
}

func TestRevealRequiredWithoutAProtectorIsAFaultNotARejection(t *testing.T) {
	f := setup(t, true)
	if _, err := f.handle(t, "c1", "Register", alice); err != nil {
		t.Fatal(err)
	}
	decider.Register(f.reg, "customer", customer()) // the protector goes missing
	_, err := f.handle(t, "c1", "CheckDomain", `{}`)
	if !errors.Is(err, decider.ErrRevealRequired) || decider.IsRejection(err) {
		t.Fatalf("want an unrejected ErrRevealRequired, got %v", err)
	}
}

func TestErasedSubjectIsRefusedAndAReturningPersonIsANewSubject(t *testing.T) {
	f := setup(t, true)
	ctx := context.Background()
	if _, err := f.handle(t, "c1", "Register", alice); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Handle(ctx, pii.SubjectAggregate, "c1", decider.Command{Name: pii.EraseSubjectCommand}); err != nil {
		t.Fatal(err)
	}
	_, err := f.handle(t, "c1b", "Register", alice) // same subject id, new stream
	if !kms.IsSubjectErased(err) || decider.IsRejection(err) {
		t.Fatalf("new pii for an erased subject must be refused (410), got %v", err)
	}
	if _, err := f.handle(t, "c2", "Register", `{"customerId":"c2","email":"alice@example.com"}`); err != nil {
		t.Fatalf("a returning person, as a new subject, is fine: %v", err)
	}
}

func TestErasedReadIsARejectionNotARetry(t *testing.T) {
	f := setup(t, true)
	ctx := context.Background()
	if _, err := f.handle(t, "c1", "Register", alice); err != nil {
		t.Fatal(err)
	}
	_ = f.kms.DestroyKey(ctx, "c1")
	// A fresh process (cold cache). A warm cache would still answer until the
	// SubjectErased event reaches its evictor: erasure has a bounded lag.
	decider.RegisterProtected(f.reg, "customer", customer(), customerProtector{pii.Deps{KMS: f.kms, Cache: pii.NewCache(0)}})
	_, err := f.handle(t, "c1", "CheckDomain", `{}`)
	var re *pii.RedactedError
	if !errors.As(err, &re) || !decider.IsRejection(err) {
		t.Fatalf("an erased value read by decide is final: %v", err)
	}
}

// ---- seal at intake ----

func TestSealedCommandPassesThroughWithItsCiphertextAndNoRoundTrip(t *testing.T) {
	f := setup(t, true)
	ctx := context.Background()
	sealed, err := f.reg.Seal(ctx, "customer", decider.Command{Name: "Import", Payload: json.RawMessage(alice)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "alice") {
		t.Fatalf("sealed payload still holds plaintext: %s", sealed)
	}
	encBefore, _, _ := f.kms.Calls()
	evs, err := f.handle(t, "c1", "Import", string(sealed))
	if err != nil {
		t.Fatal(err)
	}
	enc, dec, _ := f.kms.Calls()
	if enc != encBefore || dec != 0 {
		t.Fatalf("a sealed value decide never reads costs nothing more: +%d enc, %d dec", enc-encBefore, dec)
	}
	var in, out struct {
		Email json.RawMessage `json:"email"`
	}
	_ = json.Unmarshal(sealed, &in)
	_ = json.Unmarshal(evs[0].Data, &out)
	if string(in.Email) != string(out.Email) {
		t.Fatalf("the event must carry the intake ciphertext byte for byte:\n%s\n%s", in.Email, out.Email)
	}
}

func TestSealedCommandThatDecideReadsIsAWarmReveal(t *testing.T) {
	f := setup(t, true)
	ctx := context.Background()
	sealed, _ := f.reg.Seal(ctx, "customer", decider.Command{Name: "Register", Payload: json.RawMessage(alice)})
	if _, err := f.handle(t, "c1", "Register", string(sealed)); err != nil {
		t.Fatalf("decide reads the sealed email via reveal + second decide: %v", err)
	}
	if _, dec, _ := f.kms.Calls(); dec != 0 {
		t.Fatalf("intake primed the cache, so no decrypt call: %d", dec)
	}
	bad, _ := f.reg.Seal(ctx, "customer", decider.Command{Name: "Register", Payload: json.RawMessage(`{"customerId":"c2","email":"nope"}`)})
	if _, err := f.handle(t, "c2", "Register", string(bad)); !decider.IsRejection(err) {
		t.Fatalf("decide still judges the revealed plaintext: %v", err)
	}
}

func TestSealedUnderOneSubjectStoredUnderAnotherFailsClosed(t *testing.T) {
	f := setup(t, true)
	sealed, _ := f.reg.Seal(context.Background(), "customer", decider.Command{Name: "Import", Payload: json.RawMessage(alice)})
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(sealed, &obj)
	obj["customerId"] = json.RawMessage(`"someone-else"`)
	tampered, _ := json.Marshal(obj)
	_, err := f.handle(t, "c1", "Import", string(tampered))
	if err == nil || !strings.Contains(err.Error(), "wrong key") {
		t.Fatalf("data sealed for one subject must never be stored under another: %v", err)
	}
}

func TestSealRefusesAnErasedSubjectAtIntake(t *testing.T) {
	f := setup(t, true)
	ctx := context.Background()
	_, _ = f.reg.Handle(ctx, pii.SubjectAggregate, "c1", decider.Command{Name: pii.EraseSubjectCommand})
	_, err := f.reg.Seal(ctx, "customer", decider.Command{Name: "Register", Payload: json.RawMessage(alice)})
	if !kms.IsSubjectErased(err) {
		t.Fatalf("want SubjectErasedError at intake, got %v", err)
	}
}

// TestBatchingQueueNeverHoldsPlaintext drives the real batching writer and
// reads commandqueue.db directly.
func TestBatchingQueueNeverHoldsPlaintext(t *testing.T) {
	f := setup(t, true)
	ctx := context.Background()
	qpath := filepath.Join(t.TempDir(), "commandqueue.db")
	queue, err := commandqueue.Open(qpath)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	w := batching.NewWriter(f.store, queue, f.reg, nil)
	meta := map[string]any{"now": time.Now().UTC().Format(time.RFC3339)}
	_, wait, err := w.Enqueue(ctx, "customer", "c1", "Register", json.RawMessage(alice), meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	out := <-wait
	if out.Err != nil {
		t.Fatal(out.Err)
	}
	db, err := sql.Open("sqlite", qpath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT payload FROM command_queue`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var p string
		_ = rows.Scan(&p)
		n++
		if strings.Contains(p, "alice") {
			t.Fatalf("plaintext reached commandqueue.db: %s", p)
		}
	}
	if n != 1 {
		t.Fatalf("want the one queued row, got %d", n)
	}
	if strings.Contains(string(out.Events[0].Data), "alice") {
		t.Fatal("nor the log")
	}
}
