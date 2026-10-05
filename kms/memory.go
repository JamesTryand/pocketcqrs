package kms

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// Memory is a Client with no facade behind it, for tests, scenario
// verification and local dry runs. It honours the facade's contract (keys
// must be ensured before encrypt; a destroyed key makes every ciphertext
// under it KeyDestroyed; destroy is idempotent; an erased subject is
// tombstoned, so EnsureKey refuses it) but its "ciphertext" is a reversible,
// self-describing wrapper — "inmem:{subject}:{base64 plaintext}" — not
// encryption. Never use it in a real host: a host that stores personal data
// and has no KMS_FACADE_URL must refuse to start, not fall back to this.
//
// Index keys are real HMAC-SHA256 keys, random per name and version and
// held only in this value, with output in the facade's "vault:vN:<base64>"
// shape, so hashes are deterministic within one Memory. RotateIndexKey
// stands in for ops' vault.sh rotate-index-key.
type Memory struct {
	mu        sync.Mutex
	keys      map[string]bool
	erasures  []string
	indexKeys map[string][][]byte

	EncryptCalls      int
	DecryptBatchCalls int
	HMACCalls         int
}

// NewMemory returns an empty in-memory facade.
func NewMemory() *Memory {
	return &Memory{keys: map[string]bool{}, indexKeys: map[string][][]byte{}}
}

var _ Client = (*Memory)(nil)

// Calls returns the call counters under the lock (for tests that read them
// while other goroutines may still be calling).
func (m *Memory) Calls() (encrypt, decryptBatch, hmacs int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.EncryptCalls, m.DecryptBatchCalls, m.HMACCalls
}

func (m *Memory) erased(id string) bool {
	for _, e := range m.erasures {
		if e == id {
			return true
		}
	}
	return false
}

func (m *Memory) EnsureKey(_ context.Context, subjectID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.erased(subjectID) {
		return &SubjectErasedError{SubjectID: subjectID}
	}
	m.keys[subjectID] = true
	return nil
}

func seal(subjectID string, plaintext []byte) string {
	return "inmem:" + subjectID + ":" + base64.StdEncoding.EncodeToString(plaintext)
}

func unseal(subjectID, ciphertext string) ([]byte, error) {
	prefix := "inmem:" + subjectID + ":"
	if !strings.HasPrefix(ciphertext, prefix) {
		return nil, &ProtocolError{Msg: fmt.Sprintf("ciphertext %q was not produced for subject %q by this client", ciphertext, subjectID)}
	}
	p, err := base64.StdEncoding.DecodeString(ciphertext[len(prefix):])
	if err != nil {
		return nil, &ProtocolError{Msg: "in-memory ciphertext is not base64", Err: err}
	}
	return p, nil
}

func (m *Memory) Encrypt(_ context.Context, subjectID string, plaintext []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.EncryptCalls++
	if !m.keys[subjectID] {
		return "", &KeyNotFoundError{SubjectID: subjectID}
	}
	return seal(subjectID, plaintext), nil
}

func (m *Memory) Decrypt(_ context.Context, subjectID, ciphertext string) ([]byte, KeyState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.keys[subjectID] {
		return nil, KeyDestroyed, nil
	}
	p, err := unseal(subjectID, ciphertext)
	return p, KeyFound, err
}

func (m *Memory) EncryptBatch(_ context.Context, subjectID string, plaintexts [][]byte) ([]BatchItem, error) {
	if err := checkBatch(len(plaintexts), "plaintexts"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.EncryptCalls++
	if !m.keys[subjectID] {
		return nil, &KeyNotFoundError{SubjectID: subjectID}
	}
	out := make([]BatchItem, len(plaintexts))
	for i, p := range plaintexts {
		out[i] = BatchItem{Value: []byte(seal(subjectID, p))}
	}
	return out, nil
}

func (m *Memory) DecryptBatch(_ context.Context, subjectID string, ciphertexts []string) ([]BatchItem, KeyState, error) {
	if err := checkBatch(len(ciphertexts), "ciphertexts"); err != nil {
		return nil, KeyFound, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DecryptBatchCalls++
	if !m.keys[subjectID] {
		return nil, KeyDestroyed, nil
	}
	out := make([]BatchItem, len(ciphertexts))
	for i, c := range ciphertexts {
		p, err := unseal(subjectID, c)
		if err != nil {
			return nil, KeyFound, err
		}
		out[i] = BatchItem{Value: p}
	}
	return out, KeyFound, nil
}

func (m *Memory) DestroyKey(_ context.Context, subjectID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.keys, subjectID)
	m.erasures = append(m.erasures, subjectID)
	return nil
}

// ListErasures pages the erasure ledger. An after past the end restarts from
// 0, as the facade does after its ledger is rebuilt.
func (m *Memory) ListErasures(_ context.Context, after int64, limit int) (ErasurePage, error) {
	if after < 0 {
		return ErasurePage{}, fmt.Errorf("kms: list erasures: after must be non-negative (got %d)", after)
	}
	if err := checkBatch(limit, "limit"); err != nil {
		return ErasurePage{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	start := int(after)
	if after > int64(len(m.erasures)) {
		start = 0
	}
	end := min(start+limit, len(m.erasures))
	page := append([]string{}, m.erasures[start:end]...)
	return ErasurePage{SubjectIDs: page, Next: int64(start + len(page))}, nil
}

func newIndexKey() []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return k
}

func (m *Memory) EnsureIndexKey(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.indexKeys[name]; !ok {
		m.indexKeys[name] = [][]byte{newIndexKey()}
	}
	return nil
}

func (m *Memory) IndexKeyVersion(_ context.Context, name string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.indexKeys[name]
	if !ok {
		return 0, &IndexKeyNotFoundError{Name: name}
	}
	return len(v), nil
}

// RotateIndexKey adds a new version of the named index key; earlier
// versions stay usable, as in Transit.
func (m *Memory) RotateIndexKey(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.indexKeys[name]
	if !ok {
		return &IndexKeyNotFoundError{Name: name}
	}
	m.indexKeys[name] = append(v, newIndexKey())
	return nil
}

// indexKey resolves keyVersion 0 to the latest; anything outside
// 1..latest is the facade's 400.
func (m *Memory) indexKey(name string, keyVersion int) (int, []byte, error) {
	versions, ok := m.indexKeys[name]
	if !ok {
		return 0, nil, &IndexKeyNotFoundError{Name: name}
	}
	v := keyVersion
	if v == 0 {
		v = len(versions)
	}
	if v < 1 || v > len(versions) {
		return 0, nil, &StatusError{Op: fmt.Sprintf("hmac (index key %q has no version %d)", name, v), Status: http.StatusBadRequest}
	}
	return v, versions[v-1], nil
}

func hmacString(version int, key, input []byte) string {
	h := hmac.New(sha256.New, key)
	h.Write(input)
	return fmt.Sprintf("vault:v%d:%s", version, base64.StdEncoding.EncodeToString(h.Sum(nil)))
}

func (m *Memory) HMAC(_ context.Context, name string, input []byte, keyVersion int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.HMACCalls++
	v, key, err := m.indexKey(name, keyVersion)
	if err != nil {
		return "", err
	}
	return hmacString(v, key, input), nil
}

func (m *Memory) HMACBatch(_ context.Context, name string, inputs [][]byte, keyVersion int) ([]BatchItem, error) {
	if err := checkBatch(len(inputs), "inputs"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.HMACCalls++
	v, key, err := m.indexKey(name, keyVersion)
	if err != nil {
		return nil, err
	}
	out := make([]BatchItem, len(inputs))
	for i, in := range inputs {
		out[i] = BatchItem{Value: []byte(hmacString(v, key, in))}
	}
	return out, nil
}
