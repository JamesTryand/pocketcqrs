package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// piiDomain is a customer aggregate whose registration event carries an
// email as personal data (subject customerId), projected into a read model.
func piiDomain() Domain {
	email := Field{Name: "email", Type: "text", PII: true, PIISubject: "customerId"}
	return Domain{
		Aggregate: "customer",
		Commands: []Command{{
			Name: "RegisterCustomer", Once: true,
			Fields: []Field{{Name: "customerId", Type: "text"}, email, {Name: "name", Type: "text"}},
			Events: []Event{{Name: "CustomerRegistered",
				Fields: []Field{{Name: "customerId", Type: "text"}, email, {Name: "name", Type: "text"}}}},
		}},
		ReadModels: []ReadModel{{
			Collection: "customers", Key: "customerId",
			Fields: []Field{{Name: "customerId", Type: "text"}, email, {Name: "name", Type: "text"}},
			On:     []string{"CustomerRegistered"},
			Filters: []ReadModelFilter{
				{Param: "email", Field: "email", Kind: FilterMatch, Mode: "exact", Normalize: "email"},
				{Param: "name", Field: "name", Kind: FilterMatch, Mode: "prefix", MinPrefixLength: 2},
			},
		}},
	}
}

func TestPIIDomainGeneratesAProtectorAndEnvelopeColumns(t *testing.T) {
	files, err := piiDomain().GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	src := map[string]string{}
	for _, f := range files {
		src[f.Name] = f.Source
	}
	dec := src["customer.go"]
	for _, want := range []string{
		"Email pii.Value[string]",
		"type CustomerRegisteredData struct",
		"Payload: &payload",
		"pii.ResolveIn(cmd, &payload.Email)",
		"func NewProtector(deps pii.Deps) *Protector",
		`pii.ProtectField(ctx, p.deps, pl.Email, pl.CustomerId, "CustomerRegistered.email", "customerId")`,
		`pii.SealField{{Field: "email", Subject: "customerId"}}`,
		"decider.RegisterProtected(registry",
	} {
		// gofmt aligns struct fields, so compare with runs of space collapsed
		if !strings.Contains(strings.Join(strings.Fields(dec), " "), want) {
			t.Errorf("decider is missing %q:\n%s", want, dec)
		}
	}
	proj := src["customers.go"]
	if !strings.Contains(proj, "rec.Set(name, string(env))") || !strings.Contains(proj, `[]string{"email"}`) {
		t.Errorf("the email column must store the envelope text:\n%s", proj)
	}
	if strings.Contains(proj, `[]string{"email", "name"}`) {
		t.Errorf("the email column must not be merged as a plain value:\n%s", proj)
	}
	q := strings.Join(strings.Fields(src["customers_query.go"]), " ")
	for _, want := range []string{
		"var CustomersQuery = queryroute.Spec{",
		`Columns: []string{"customerId", "email", "name"}`,
		`PII: map[string]string{"email": "customerId"}`,
		`SelfSubject: "customerId"`,
		"nobody but superusers", // no requiredRole declared: closed by default, and said so
		"var CustomersSearch = searchindex.Spec{",
		`{Param: "email", Field: "email", Mode: "exact", Normalize: "email", PII: true}`,
		`{Param: "name", Field: "name", Mode: "prefix", MinPrefix: 2}`,
		"Match: CustomersSearch.Fields,",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query spec is missing %q:\n%s", want, src["customers_query.go"])
		}
	}
}

func TestQuerySpecOmitsSelfAccessForMixedSubjectsAndCarriesTheRole(t *testing.T) {
	d := piiDomain()
	rm := &d.ReadModels[0]
	rm.RequiredRole = []string{"support"}
	rm.Fields = append(rm.Fields, Field{Name: "courierId", Type: "text"},
		Field{Name: "courierPhone", Type: "text", PII: true, PIISubject: "courierId"})
	src := queryGo("customer", *rm, []string{"CustomerRegistered"})
	if strings.Contains(src, "SelfSubject") {
		t.Errorf("two subjects per row: no self-access:\n%s", src)
	}
	if !strings.Contains(src, `RequiredRole: []string{"support"}`) {
		t.Errorf("requiredRole must reach the spec:\n%s", src)
	}
}

func TestPIIConflictAcrossEventsIsRefused(t *testing.T) {
	d := piiDomain()
	d.Commands = append(d.Commands, Command{
		Name: "Touch", RequiresExisting: true,
		Events: []Event{{Name: "CustomerTouched", Fields: []Field{{Name: "email", Type: "text"}}}},
	})
	if _, err := d.GenerateGo(); err == nil || !strings.Contains(err.Error(), "email") {
		t.Fatalf("email pii on one event and plaintext on another must be refused: %v", err)
	}
}

// generatedPIIBehaviour runs inside the throwaway module, in the generated
// package, against the real registry and an in-memory key service.
const generatedPIIBehaviour = `package customer

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/kms"
	"github.com/jamestryand/pocketcqrs/pii"
)

func setup(t *testing.T, protected bool) (*decider.Registry, *events.Store) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	reg := decider.NewRegistry(store)
	deps := pii.Deps{KMS: kms.NewMemory(), Cache: pii.NewCache(0), Subjects: pii.StoreSubjectStatus{Loader: store}}
	if protected {
		decider.RegisterProtected(reg, "customer", Customer(), NewProtector(deps))
	} else {
		decider.Register(reg, "customer", Customer())
	}
	pii.RegisterDataSubjects(reg)
	return reg, store
}

const alice = ` + "`" + `{"customerId":"c1","email":"alice@example.com","name":"Alice"}` + "`" + `

func TestGeneratedProtectorEncryptsBeforeAppend(t *testing.T) {
	reg, _ := setup(t, true)
	evs, err := reg.Handle(context.Background(), "customer", "c1", decider.Command{Name: "RegisterCustomer", Payload: json.RawMessage(alice)})
	if err != nil {
		t.Fatal(err)
	}
	data := string(evs[0].Data)
	if strings.Contains(data, "alice@") || !strings.Contains(data, "$pii") || !strings.Contains(data, "Alice") {
		t.Fatalf("email sealed, name plain: %s", data)
	}
}

func TestGeneratedSealerSealsAtIntake(t *testing.T) {
	reg, _ := setup(t, true)
	ctx := context.Background()
	sealed, err := reg.Seal(ctx, "customer", decider.Command{Name: "RegisterCustomer", Payload: json.RawMessage(alice)})
	if err != nil || strings.Contains(string(sealed), "alice@") {
		t.Fatalf("sealed: %s %v", sealed, err)
	}
	if _, err := reg.Handle(ctx, "customer", "c1", decider.Command{Name: "RegisterCustomer", Payload: sealed}); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedDeciderWithoutProtectorFailsClosed(t *testing.T) {
	reg, store := setup(t, false)
	_, err := reg.Handle(context.Background(), "customer", "c1", decider.Command{Name: "RegisterCustomer", Payload: json.RawMessage(alice)})
	if err == nil || decider.IsRejection(err) {
		t.Fatalf("plaintext must never be appended: %v", err)
	}
	if evs, _ := store.LoadStream(context.Background(), "customer", "c1"); len(evs) != 0 {
		t.Fatal("nothing written")
	}
}

func TestGeneratedEvolveReadsTheEnvelopeAsPending(t *testing.T) {
	reg, store := setup(t, true)
	ctx := context.Background()
	if _, err := reg.Handle(ctx, "customer", "c1", decider.Command{Name: "RegisterCustomer", Payload: json.RawMessage(alice)}); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.LoadStream(ctx, "customer", "c1")
	s, err := Customer().Evolve(State{}, evs[0])
	if err != nil {
		t.Fatal(err)
	}
	if s.Email.State() != pii.Pending || s.Name != "Alice" {
		t.Fatalf("stored email folds as pending ciphertext: %v %q", s.Email.State(), s.Name)
	}
}
`

// TestGeneratedPIIDomainCompilesAndProtects is the end-to-end proof: the
// generated decider, protector and projection compile against real
// pocketcqrs packages, and the generated protector really encrypts.
func TestGeneratedPIIDomainCompilesAndProtects(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	files, err := piiDomain().GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	goMod := "module generated-pii-smoke\n\ngo 1.25.0\n\n" +
		"require github.com/jamestryand/pocketcqrs v0.0.0\n\n" +
		"replace github.com/jamestryand/pocketcqrs => " + repoRoot + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.Name), []byte(f.Source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "behaviour_test.go"), []byte(generatedPIIBehaviour), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir, tidy.Env = dir, env
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy failed: %v\n%s", err, out)
	}
	test := exec.Command("go", "test", "./...")
	test.Dir, test.Env = dir, env
	if out, err := test.CombinedOutput(); err != nil {
		t.Fatalf("generated pii domain failed:\n%s", out)
	}
}
