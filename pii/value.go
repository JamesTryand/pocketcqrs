// Package pii is personal data, crypto-shredded: every value marked pii in
// an EventModeling document is encrypted under its data subject's key (held
// by the key-management facade, package kms) before it reaches the
// append-only log, and erasing the subject destroys the key, which makes
// every copy unreadable — the log is never edited. It is the Go port of
// dotnetcqrs's DotnetCqrs.Crypto runtime, and keeps its rules:
//
//   - Lazy: a stored value decodes to a Pending Value carrying ciphertext;
//     nothing is decrypted until something reveals it.
//   - Batched: reveals queue in a Buffer and flush as one decrypt-batch call
//     per subject.
//   - Fail closed: a Fresh (plaintext) Value refuses to marshal, so a
//     mis-wired host fails at append instead of writing plaintext.
//   - Erasure is terminal per subject: a returning person is a new subject
//     with a new id (see DataSubject).
//
// The design record is the notebook's platform/eventmodeling-codegen
// findings.md ("Write-path design", "Read-side design", "Milestone G").
package pii

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/kms"
)

// State is where a Value is in its lifecycle.
type State int

const (
	// Unset is the zero Value: no value at all (an absent optional field).
	// It marshals as null.
	Unset State = iota
	// Fresh is plaintext that has just entered the system (a command field)
	// and has not been encrypted yet. Get reads it; it refuses to marshal,
	// so it can never reach the event store by accident.
	Fresh
	// Pending carries ciphertext and a subject id; nothing decrypted yet.
	// Constructing one costs no facade traffic. Get returns an error
	// wrapping decider.ErrRevealRequired.
	Pending
	// Redacted means the subject's key has been destroyed (erased) — the
	// correct terminal state for an erased subject's data, not a fault.
	Redacted
	// Known has both the value and its ciphertext (revealed, or freshly
	// encrypted), so it marshals without another round trip.
	Known
)

func (s State) String() string {
	return [...]string{"unset", "fresh", "pending", "redacted", "known"}[s]
}

// Value is one personal-data field. T is the field's folded Go type
// (string, float64, bool, json.RawMessage, ...); the encrypt/decrypt codec
// is encoding/json round-tripping of T.
//
// On the wire a Value reads either a bare JSON value (Fresh: a command's
// plaintext) or the envelope {"$pii":{"s":subject,"c":ciphertext}}
// (Pending; "c": null is Redacted), and writes only the envelope. No code
// path here produces plaintext the facade didn't hand back or a command
// didn't carry in.
type Value[T any] struct {
	state   State
	subject string
	cipher  string
	v       T
}

// NewFresh wraps plaintext that has just arrived and must be encrypted
// before it can be stored. A decide may read it freely.
func NewFresh[T any](v T) Value[T] { return Value[T]{state: Fresh, v: v} }

// FromCiphertext wraps a ciphertext read back from storage. Costs nothing
// until revealed.
func FromCiphertext[T any](subjectID, ciphertext string) Value[T] {
	return Value[T]{state: Pending, subject: subjectID, cipher: ciphertext}
}

// NewRedacted is a value whose subject's key is known to be destroyed.
func NewRedacted[T any](subjectID string) Value[T] {
	return Value[T]{state: Redacted, subject: subjectID}
}

func (p Value[T]) State() State               { return p.state }
func (p Value[T]) SubjectID() string          { return p.subject }
func (p Value[T]) Ciphertext() string         { return p.cipher }
func (p Value[T]) IsZero() bool               { return p.state == Unset }
func (p Value[T]) String() string             { return "pii." + p.state.String() } // never the plaintext
func (p Value[T]) GoString() string           { return p.String() }
func (p Value[T]) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(p.String())) }

// RevealRequiredError is Get on a Pending value. errors.Is matches
// decider.ErrRevealRequired, which the decider shell turns into one batched
// reveal and a second decide.
type RevealRequiredError struct{ SubjectID string }

func (e *RevealRequiredError) Error() string {
	return fmt.Sprintf("pii: value for subject %q has not been revealed yet", e.SubjectID)
}
func (e *RevealRequiredError) Is(target error) bool { return target == decider.ErrRevealRequired }

// RedactedError is Get on a value whose subject has been erased. No reveal
// can help, so the shell does not retry; a decide that expects erasure as a
// normal branch checks State() == Redacted instead of calling Get.
type RedactedError struct{ SubjectID string }

func (e *RedactedError) Error() string {
	return fmt.Sprintf("pii: personal data for subject %q has been erased (key destroyed)", e.SubjectID)
}

// ErrUnset is Get on an Unset value.
var ErrUnset = errors.New("pii: no value")

// Get returns the plaintext of a Fresh or Known value. Pending returns a
// *RevealRequiredError; Redacted a *RedactedError; Unset ErrUnset.
func (p Value[T]) Get() (T, error) {
	var zero T
	switch p.state {
	case Fresh, Known:
		return p.v, nil
	case Pending:
		return zero, &RevealRequiredError{SubjectID: p.subject}
	case Redacted:
		return zero, &RedactedError{SubjectID: p.subject}
	}
	return zero, ErrUnset
}

// Encrypt encrypts a Fresh value under subjectID's key (ensuring the key
// exists), returning a Known value that carries the ciphertext. Any other
// state returns itself — already encrypted, or nothing to encrypt. cache,
// when non-nil, is primed with the plaintext just encrypted, so the reveal
// that follows a sealed command (see the batching writer) is a hit.
func (p Value[T]) Encrypt(ctx context.Context, c kms.Client, subjectID string, cache *Cache) (Value[T], error) {
	if p.state != Fresh {
		return p, nil
	}
	if subjectID == "" {
		return p, errors.New("pii: refusing to encrypt under an empty subject id")
	}
	plain, err := json.Marshal(p.v)
	if err != nil {
		return p, fmt.Errorf("pii: encode: %w", err)
	}
	if err := c.EnsureKey(ctx, subjectID); err != nil {
		return p, err
	}
	cipher, err := c.Encrypt(ctx, subjectID, plain)
	if err != nil {
		return p, err
	}
	if cache != nil {
		cache.Put(subjectID, cipher, plain)
	}
	return Value[T]{state: Known, subject: subjectID, cipher: cipher, v: p.v}, nil
}

func decode[T any](plain []byte) (T, error) {
	var v T
	if bytes.Equal(bytes.TrimSpace(plain), []byte("null")) {
		return v, &kms.ProtocolError{Msg: "decrypted plaintext was null"}
	}
	if err := json.Unmarshal(plain, &v); err != nil {
		return v, &kms.ProtocolError{Msg: "decrypted plaintext does not decode as the field's type", Err: err}
	}
	return v, nil
}

const (
	envelopeKey = "$pii"
	subjectKey  = "s"
	cipherKey   = "c"
)

// MarshalJSON writes the envelope. A Fresh value is refused: that is the
// guarantee that plaintext never reaches the log through a mis-wired host
// (no protector registered, or one that missed a field).
func (p Value[T]) MarshalJSON() ([]byte, error) {
	switch p.state {
	case Unset:
		return []byte("null"), nil
	case Fresh:
		return nil, errors.New("pii: refusing to marshal an unencrypted value — the aggregate has no personal-data " +
			"protector, or decide produced a fresh value the protector did not encrypt")
	}
	body := map[string]any{subjectKey: p.subject, cipherKey: nil}
	if p.state != Redacted {
		body[cipherKey] = p.cipher
	}
	return json.Marshal(map[string]any{envelopeKey: body})
}

// UnmarshalJSON reads null as Unset, the envelope as Pending (Redacted when
// "c" is missing or not a string), and anything else as Fresh plaintext —
// including an object whose first key isn't "$pii" (a json-typed field
// carrying an object). An object whose first key IS "$pii" must be exactly
// the envelope: MarshalJSON never adds a sibling, so extra keys mean a
// malformed or tampered value, and it is refused rather than read as
// ciphertext with the rest silently dropped.
func (p *Value[T]) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		*p = Value[T]{}
		return nil
	}
	if isEnvelope, err := startsWithEnvelope(data); err != nil {
		return err
	} else if !isEnvelope {
		var v T
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		*p = NewFresh(v)
		return nil
	}
	subject, cipher, ok, err := ParseEnvelope(data)
	if err != nil {
		return err
	}
	if !ok {
		*p = NewRedacted[T](subject)
		return nil
	}
	*p = FromCiphertext[T](subject, cipher)
	return nil
}

// startsWithEnvelope reports whether data is an object whose first key is
// "$pii".
func startsWithEnvelope(data []byte) (bool, error) {
	if len(data) == 0 || data[0] != '{' {
		return false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if _, err := dec.Token(); err != nil {
		return false, err
	}
	if !dec.More() {
		return false, nil
	}
	tok, err := dec.Token()
	if err != nil {
		return false, err
	}
	key, _ := tok.(string)
	return key == envelopeKey, nil
}

// ParseEnvelope reads {"$pii":{"s":subject,"c":ciphertext}}. hasCipher is
// false for a redacted value ("c" null or missing). It errors unless data is
// exactly one envelope with a string subject. Exported for the read side,
// which handles envelopes without knowing a field's Go type.
func ParseEnvelope(data []byte) (subject, cipher string, hasCipher bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	malformed := func(why string) error {
		return fmt.Errorf("pii: malformed %s envelope: %s", envelopeKey, why)
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return "", "", false, malformed("not an object")
	}
	if t, err := dec.Token(); err != nil || t != envelopeKey {
		return "", "", false, malformed(`first key is not "$pii"`)
	}
	var body map[string]json.RawMessage
	if err := dec.Decode(&body); err != nil {
		return "", "", false, malformed("body is not an object")
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return "", "", false, malformed(`"$pii" must be its only key`)
	}
	if err := json.Unmarshal(body[subjectKey], &subject); err != nil || body[subjectKey] == nil {
		return "", "", false, malformed(`"s" must be a string`)
	}
	if raw, ok := body[cipherKey]; ok && len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &cipher); err != nil {
			return "", "", false, malformed(`"c" is not a valid string`)
		}
		return subject, cipher, true, nil
	}
	return subject, "", false, nil
}

// IsEnvelope reports whether data is a JSON object whose first key is
// "$pii" — what the gateway refuses from an external caller.
func IsEnvelope(data []byte) bool {
	ok, err := startsWithEnvelope(bytes.TrimSpace(data))
	return ok && err == nil
}
