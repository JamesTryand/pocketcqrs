// Package kms is the caller side of the key-management facade
// (journal.bureau.tryand.uk/kms, docs/facade-api-contract.md): per-subject
// keys for crypto-shredding personal data, and named index keys for blind
// (HMAC) search indexes. Keys never leave the facade; this package only asks
// it to encrypt, decrypt, hash and destroy.
//
// It is the Go port of dotnetcqrs's DotnetCqrs.Crypto client layer
// (IKmsClient, KmsClient, InMemoryKmsClient) and keeps the same contract
// decisions: a destroyed key is an expected decrypt outcome (KeyDestroyed),
// not an error; a missing key on encrypt is a programming error; the facade's
// 409 on ensuring an erased subject's key is ErrSubjectErased; anything the
// facade answers that this client can't interpret is unavailable, not a
// refusal (IsUnavailable).
package kms

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
)

// MaxBatchItems is the facade's per-call item limit for every batch call.
const MaxBatchItems = 1000

// KeyState is the outcome of a decrypt: the key was there, or it has been
// destroyed. Destroyed is the correct, terminal answer for an erased
// subject's data — never retry it.
type KeyState int

const (
	KeyFound KeyState = iota
	KeyDestroyed
)

// BatchItem is one result of a batch call, order-preserving with the input.
// Exactly one of Value and Err is set; a per-item error does not fail the
// batch. Value is the ciphertext (encrypt-batch), the plaintext
// (decrypt-batch) or the hmac (hmac-batch).
type BatchItem struct {
	Value []byte
	Err   string
}

// Succeeded reports whether the facade returned a value for this item.
func (i BatchItem) Succeeded() bool { return i.Err == "" }

// ErasurePage is one page of the facade's erasure ledger (GET
// /v1/erasures): subject ids whose keys were destroyed, oldest first, and the
// cursor to pass as after for the next page.
type ErasurePage struct {
	SubjectIDs []string
	Next       int64
}

// Client is everything the platform asks the facade for. Implementations:
// HTTPClient (a real facade) and Memory (tests and scenario verification
// only — never a production host).
type Client interface {
	// EnsureKey creates subjectID's key if it doesn't exist (idempotent).
	// Returns a *SubjectErasedError when the facade has tombstoned the
	// subject: an erased subject never gets a key again.
	EnsureKey(ctx context.Context, subjectID string) error
	// Encrypt returns the ciphertext. A *KeyNotFoundError means EnsureKey
	// was skipped.
	Encrypt(ctx context.Context, subjectID string, plaintext []byte) (string, error)
	// Decrypt returns KeyDestroyed (and no plaintext) for an erased subject.
	Decrypt(ctx context.Context, subjectID, ciphertext string) ([]byte, KeyState, error)
	// EncryptBatch encrypts 1..MaxBatchItems values under one subject's key.
	EncryptBatch(ctx context.Context, subjectID string, plaintexts [][]byte) ([]BatchItem, error)
	// DecryptBatch is the mirror of EncryptBatch; KeyDestroyed for the whole
	// call when the subject's key is gone.
	DecryptBatch(ctx context.Context, subjectID string, ciphertexts []string) ([]BatchItem, KeyState, error)
	// DestroyKey is erasure. Idempotent.
	DestroyKey(ctx context.Context, subjectID string) error
	// ListErasures pages the facade's erasure ledger from after (exclusive),
	// limit 1..MaxBatchItems.
	ListErasures(ctx context.Context, after int64, limit int) (ErasurePage, error)

	// EnsureIndexKey creates the named HMAC key if it doesn't exist.
	EnsureIndexKey(ctx context.Context, name string) error
	// IndexKeyVersion returns the named key's latest version.
	IndexKeyVersion(ctx context.Context, name string) (int, error)
	// HMAC hashes input under the named key at keyVersion (0 = latest),
	// returning the facade's "vault:vN:<base64>" string.
	HMAC(ctx context.Context, name string, input []byte, keyVersion int) (string, error)
	// HMACBatch hashes 1..MaxBatchItems inputs at one key version (0 =
	// latest). Each item's Value is the "vault:vN:..." string's bytes.
	HMACBatch(ctx context.Context, name string, inputs [][]byte, keyVersion int) ([]BatchItem, error)
}

// SubjectErasedError: a command would store fresh personal data for a
// subject whose lifecycle has ended. A returning person is a new subject with
// a new id, never this one. Permanent (the gateway answers 410), unlike a
// concurrency conflict.
type SubjectErasedError struct{ SubjectID string }

func (e *SubjectErasedError) Error() string {
	return fmt.Sprintf("data subject %q has been erased; a returning subject needs a new id, not this one", e.SubjectID)
}

// KeyNotFoundError: an encrypt-shaped call hit a subject with no key yet —
// the caller skipped EnsureKey. A programming error to fix at the call site
// (the gateway answers 500), not an expected runtime state; contrast
// KeyDestroyed, decrypt's own expected "gone" outcome.
type KeyNotFoundError struct{ SubjectID string }

func (e *KeyNotFoundError) Error() string {
	return fmt.Sprintf("no KMS key exists for subject %q — call EnsureKey first", e.SubjectID)
}

// IndexKeyNotFoundError: an index-key call named an HMAC key that doesn't
// exist — EnsureIndexKey was skipped. A programming error, like
// KeyNotFoundError.
type IndexKeyNotFoundError struct{ Name string }

func (e *IndexKeyNotFoundError) Error() string {
	return fmt.Sprintf("no KMS index key named %q exists — call EnsureIndexKey first", e.Name)
}

// ProtocolError: the facade answered something this client can't interpret
// (a malformed body, a per-item batch error surfaced through a reveal).
// Unavailable, not a refusal: the gateway answers 503 and a reactor retries.
type ProtocolError struct {
	Msg string
	Err error
}

func (e *ProtocolError) Error() string {
	if e.Err != nil {
		return "kms: " + e.Msg + ": " + e.Err.Error()
	}
	return "kms: " + e.Msg
}

func (e *ProtocolError) Unwrap() error { return e.Err }

// StatusError is a facade status this client has no specific meaning for
// (e.g. 400 for an index key version that doesn't exist, 5xx). Unavailable.
type StatusError struct {
	Op     string
	Status int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("kms: %s: facade answered HTTP %d", e.Op, e.Status)
}

// IsUnavailable reports whether err means the facade couldn't be used right
// now — unreachable, timed out, or answering something unexpected — rather
// than that the request itself was wrong. The gateway maps it to 503 and a
// reactor retries. A KeyNotFoundError/IndexKeyNotFoundError (wiring faults)
// and a SubjectErasedError (permanent) are deliberately not unavailable.
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var pe *ProtocolError
	var se *StatusError
	var ne net.Error
	var oe *net.OpError
	switch {
	case errors.As(err, &pe), errors.As(err, &se), errors.As(err, &ne), errors.As(err, &oe):
		return true
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return true
	}
	return false
}

// IsSubjectErased reports whether err is (or wraps) a SubjectErasedError.
func IsSubjectErased(err error) bool {
	var se *SubjectErasedError
	return errors.As(err, &se)
}

func checkBatch(n int, what string) error {
	if n < 1 || n > MaxBatchItems {
		return fmt.Errorf("kms: %s must contain 1-%d items, matching the facade's own limit (got %d)", what, MaxBatchItems, n)
	}
	return nil
}
