package pii

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/kms"
)

func TestScalarReadsAsFreshAndEnvelopeAsPending(t *testing.T) {
	var fresh Value[string]
	if err := json.Unmarshal([]byte(`"alice@example.com"`), &fresh); err != nil {
		t.Fatal(err)
	}
	if v, err := fresh.Get(); fresh.State() != Fresh || err != nil || v != "alice@example.com" {
		t.Fatalf("got %v %q %v", fresh.State(), v, err)
	}

	var pending Value[string]
	if err := json.Unmarshal([]byte(`{"$pii":{"s":"s1","c":"vault:v1:abc"}}`), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.State() != Pending || pending.SubjectID() != "s1" || pending.Ciphertext() != "vault:v1:abc" {
		t.Fatalf("got %v", pending.State())
	}
	_, err := pending.Get()
	if !errors.Is(err, decider.ErrRevealRequired) {
		t.Fatalf("a pending read must ask for a reveal: %v", err)
	}
}

func TestNullCipherIsRedactedAndNullIsUnset(t *testing.T) {
	var r Value[string]
	_ = json.Unmarshal([]byte(`{"$pii":{"s":"s1","c":null}}`), &r)
	if r.State() != Redacted {
		t.Fatalf("want redacted, got %v", r.State())
	}
	var re *RedactedError
	if _, err := r.Get(); !errors.As(err, &re) || errors.Is(err, decider.ErrRevealRequired) {
		t.Fatalf("a redacted read is final, not a reveal: %v", err)
	}
	var u Value[string]
	_ = json.Unmarshal([]byte(`null`), &u)
	if u.State() != Unset || !u.IsZero() {
		t.Fatal("null must stay unset")
	}
	b, _ := json.Marshal(u)
	if string(b) != "null" {
		t.Fatalf("unset must marshal as null, got %s", b)
	}
}

func TestFreshRefusesToMarshal(t *testing.T) {
	_, err := json.Marshal(struct {
		Email Value[string] `json:"email"`
	}{NewFresh("alice@example.com")})
	if err == nil || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("plaintext must never marshal: %v", err)
	}
}

func TestStoredStatesRoundTripWithoutPlaintext(t *testing.T) {
	m := kms.NewMemory()
	known, err := NewFresh("alice@example.com").Encrypt(context.Background(), m, "s1", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []Value[string]{known, FromCiphertext[string]("s1", "c1"), NewRedacted[string]("s1")} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "alice") {
			t.Fatalf("plaintext leaked: %s", b)
		}
		var back Value[string]
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		want := v.State()
		if want == Known {
			want = Pending // stored, it is ciphertext again
		}
		if back.State() != want || back.SubjectID() != "s1" {
			t.Fatalf("%v came back as %v (%s)", v.State(), back.State(), b)
		}
	}
}

func TestJSONObjectValueIsPlaintextUnlessItIsAnEnvelope(t *testing.T) {
	var v Value[json.RawMessage]
	if err := json.Unmarshal([]byte(`{"street":"1 High St","$pii":"x"}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.State() != Fresh {
		t.Fatalf(`an object whose first key isn't "$pii" is plaintext, got %v`, v.State())
	}
	if err := json.Unmarshal([]byte(`{"$pii":{"s":"s1","c":"c1"},"extra":1}`), &v); err == nil {
		t.Fatal(`"$pii" with a sibling must be refused`)
	}
	if err := json.Unmarshal([]byte(`{"$pii":{"c":"c1"}}`), &v); err == nil {
		t.Fatal(`an envelope without a string subject must be refused`)
	}
}

func TestValueNeverPrintsItsPlaintext(t *testing.T) {
	v := NewFresh("alice@example.com")
	for _, s := range []string{fmt.Sprint(v), fmt.Sprintf("%v %+v %#v %s", v, v, v, v)} {
		if strings.Contains(s, "alice") {
			t.Fatalf("printed plaintext: %s", s)
		}
	}
}

func TestEncryptIsIdempotentOnNonFresh(t *testing.T) {
	m := kms.NewMemory()
	p := FromCiphertext[string]("s1", "c1")
	got, err := p.Encrypt(context.Background(), m, "s1", nil)
	if err != nil || got != p {
		t.Fatalf("a non-fresh value must come back unchanged: %v", err)
	}
	if enc, _, _ := m.Calls(); enc != 0 {
		t.Fatal("and must not call the facade")
	}
	if _, err := NewFresh("x").Encrypt(context.Background(), m, "", nil); err == nil {
		t.Fatal("an empty subject must be refused")
	}
}

func TestEncryptForAnErasedSubjectIsRefused(t *testing.T) {
	m := kms.NewMemory()
	ctx := context.Background()
	_ = m.DestroyKey(ctx, "s1")
	_, err := NewFresh("x").Encrypt(ctx, m, "s1", nil)
	if !kms.IsSubjectErased(err) {
		t.Fatalf("an erased subject must never be written to again: %v", err)
	}
}

// Every folded field type round-trips through encrypt and reveal.
func TestEveryFieldTypeRoundTrips(t *testing.T) {
	ctx := context.Background()
	m := kms.NewMemory()
	check := func(name string, roundTrip func() error) {
		t.Helper()
		if err := roundTrip(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	check("string", func() error { return roundTrip(ctx, m, "alice") })
	check("float64", func() error { return roundTrip(ctx, m, 41.5) })
	check("bool", func() error { return roundTrip(ctx, m, true) })
	check("json", func() error {
		return roundTripWith(ctx, m, json.RawMessage(`{"a":[1,2]}`), func(a, b json.RawMessage) bool { return bytes.Equal(a, b) })
	})
}

func roundTrip[T comparable](ctx context.Context, m kms.Client, v T) error {
	return roundTripWith(ctx, m, v, func(a, b T) bool { return a == b })
}

func roundTripWith[T any](ctx context.Context, m kms.Client, v T, eq func(a, b T) bool) error {
	enc, err := NewFresh(v).Encrypt(ctx, m, "s1", nil)
	if err != nil {
		return err
	}
	b, err := json.Marshal(enc)
	if err != nil {
		return err
	}
	var back Value[T]
	if err := json.Unmarshal(b, &back); err != nil {
		return err
	}
	buf := NewBuffer(m, nil)
	if err := Reveal(buf, &back); err != nil {
		return err
	}
	if err := buf.Flush(ctx); err != nil {
		return err
	}
	got, err := back.Get()
	if err != nil {
		return err
	}
	if !eq(got, v) {
		return fmt.Errorf("got %v want %v", got, v)
	}
	return nil
}
