package kms

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// standIn serves the facade contract over HTTP from a Memory, so the HTTP
// client is tested against the same wire shapes the real facade uses
// (docs/facade-api-contract.md), and both Clients run the same scenarios.
func standIn(t *testing.T, m *Memory) *httptest.Server {
	t.Helper()
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, err error) {
		var kn *KeyNotFoundError
		var ik *IndexKeyNotFoundError
		var se *SubjectErasedError
		var st *StatusError
		switch {
		case errors.As(err, &kn), errors.As(err, &ik):
			w.WriteHeader(http.StatusNotFound)
		case errors.As(err, &se):
			w.WriteHeader(http.StatusConflict)
		case errors.As(err, &st):
			w.WriteHeader(st.Status)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
	read := func(r *http.Request, v any) { _ = json.NewDecoder(r.Body).Decode(v) }
	unb := func(s string) []byte { b, _ := base64.StdEncoding.DecodeString(s); return b }
	ctx := context.Background()
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/subjects/{id}/key", func(w http.ResponseWriter, r *http.Request) {
		if err := m.EnsureKey(ctx, r.PathValue("id")); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /v1/subjects/{id}/key", func(w http.ResponseWriter, r *http.Request) {
		_ = m.DestroyKey(ctx, r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/subjects/{id}/encrypt", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Plaintext string }
		read(r, &req)
		c, err := m.Encrypt(ctx, r.PathValue("id"), unb(req.Plaintext))
		if err != nil {
			fail(w, err)
			return
		}
		write(w, map[string]string{"ciphertext": c})
	})
	mux.HandleFunc("POST /v1/subjects/{id}/decrypt", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Ciphertext string }
		read(r, &req)
		p, state, err := m.Decrypt(ctx, r.PathValue("id"), req.Ciphertext)
		if err != nil {
			fail(w, err)
			return
		}
		if state == KeyDestroyed {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		write(w, map[string]string{"plaintext": base64.StdEncoding.EncodeToString(p)})
	})
	mux.HandleFunc("POST /v1/subjects/{id}/encrypt-batch", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Plaintexts []string }
		read(r, &req)
		in := make([][]byte, len(req.Plaintexts))
		for i, p := range req.Plaintexts {
			in[i] = unb(p)
		}
		items, err := m.EncryptBatch(ctx, r.PathValue("id"), in)
		if err != nil {
			fail(w, err)
			return
		}
		res := make([]map[string]string, len(items))
		for i, it := range items {
			res[i] = map[string]string{"ciphertext": string(it.Value)}
		}
		write(w, map[string]any{"results": res})
	})
	mux.HandleFunc("POST /v1/subjects/{id}/decrypt-batch", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Ciphertexts []string }
		read(r, &req)
		items, state, err := m.DecryptBatch(ctx, r.PathValue("id"), req.Ciphertexts)
		if err != nil {
			fail(w, err)
			return
		}
		if state == KeyDestroyed {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		res := make([]map[string]string, len(items))
		for i, it := range items {
			res[i] = map[string]string{"plaintext": base64.StdEncoding.EncodeToString(it.Value)}
		}
		write(w, map[string]any{"results": res})
	})
	mux.HandleFunc("GET /v1/erasures", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		page, err := m.ListErasures(ctx, after, limit)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		write(w, map[string]any{"subjectIds": page.SubjectIDs, "next": page.Next})
	})
	mux.HandleFunc("PUT /v1/index-keys/{name}", func(w http.ResponseWriter, r *http.Request) {
		_ = m.EnsureIndexKey(ctx, r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/index-keys/{name}", func(w http.ResponseWriter, r *http.Request) {
		v, err := m.IndexKeyVersion(ctx, r.PathValue("name"))
		if err != nil {
			fail(w, err)
			return
		}
		write(w, map[string]int{"latestVersion": v})
	})
	mux.HandleFunc("POST /v1/index-keys/{name}/hmac", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input      string
			KeyVersion *int `json:"keyVersion"`
		}
		read(r, &req)
		if req.KeyVersion != nil && *req.KeyVersion == 0 {
			w.WriteHeader(http.StatusBadRequest) // "latest" must be omitted, never 0
			return
		}
		v := 0
		if req.KeyVersion != nil {
			v = *req.KeyVersion
		}
		h, err := m.HMAC(ctx, r.PathValue("name"), unb(req.Input), v)
		if err != nil {
			fail(w, err)
			return
		}
		write(w, map[string]string{"hmac": h})
	})
	mux.HandleFunc("POST /v1/index-keys/{name}/hmac-batch", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Inputs     []string
			KeyVersion int `json:"keyVersion"`
		}
		read(r, &req)
		in := make([][]byte, len(req.Inputs))
		for i, p := range req.Inputs {
			in[i] = unb(p)
		}
		items, err := m.HMACBatch(ctx, r.PathValue("name"), in, req.KeyVersion)
		if err != nil {
			fail(w, err)
			return
		}
		res := make([]map[string]string, len(items))
		for i, it := range items {
			res[i] = map[string]string{"hmac": string(it.Value)}
		}
		write(w, map[string]any{"results": res})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// clients runs fn against a Memory directly and against the HTTP client
// talking to a stand-in facade over another Memory.
func clients(t *testing.T, fn func(t *testing.T, c Client, m *Memory)) {
	t.Run("memory", func(t *testing.T) {
		m := NewMemory()
		fn(t, m, m)
	})
	t.Run("http", func(t *testing.T) {
		m := NewMemory()
		srv := standIn(t, m)
		c, err := NewHTTPClient(srv.URL, srv.Client())
		if err != nil {
			t.Fatal(err)
		}
		fn(t, c, m)
	})
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	clients(t, func(t *testing.T, c Client, _ *Memory) {
		ctx := context.Background()
		if err := c.EnsureKey(ctx, "s1"); err != nil {
			t.Fatal(err)
		}
		ct, err := c.Encrypt(ctx, "s1", []byte(`"alice@example.com"`))
		if err != nil {
			t.Fatal(err)
		}
		p, state, err := c.Decrypt(ctx, "s1", ct)
		if err != nil || state != KeyFound || string(p) != `"alice@example.com"` {
			t.Fatalf("got %q %v %v", p, state, err)
		}
	})
}

func TestEncryptBeforeEnsureIsAWiringFault(t *testing.T) {
	clients(t, func(t *testing.T, c Client, _ *Memory) {
		_, err := c.Encrypt(context.Background(), "nobody", []byte("x"))
		var kn *KeyNotFoundError
		if !errors.As(err, &kn) || kn.SubjectID != "nobody" {
			t.Fatalf("want KeyNotFoundError, got %v", err)
		}
		if IsUnavailable(err) {
			t.Fatal("a missing key is a wiring fault, not unavailability")
		}
	})
}

func TestDestroyedKeyDecryptsAsDestroyedAndEnsureIsRefused(t *testing.T) {
	clients(t, func(t *testing.T, c Client, _ *Memory) {
		ctx := context.Background()
		_ = c.EnsureKey(ctx, "s1")
		ct, _ := c.Encrypt(ctx, "s1", []byte("1"))
		if err := c.DestroyKey(ctx, "s1"); err != nil {
			t.Fatal(err)
		}
		if err := c.DestroyKey(ctx, "s1"); err != nil {
			t.Fatalf("destroy must be idempotent: %v", err)
		}
		if _, state, err := c.Decrypt(ctx, "s1", ct); err != nil || state != KeyDestroyed {
			t.Fatalf("want KeyDestroyed, got %v %v", state, err)
		}
		if _, state, err := c.DecryptBatch(ctx, "s1", []string{ct}); err != nil || state != KeyDestroyed {
			t.Fatalf("want whole-batch KeyDestroyed, got %v %v", state, err)
		}
		err := c.EnsureKey(ctx, "s1")
		if !IsSubjectErased(err) {
			t.Fatalf("an erased subject must never get a key again: %v", err)
		}
	})
}

func TestBatchesPreserveOrder(t *testing.T) {
	clients(t, func(t *testing.T, c Client, _ *Memory) {
		ctx := context.Background()
		_ = c.EnsureKey(ctx, "s1")
		enc, err := c.EncryptBatch(ctx, "s1", [][]byte{[]byte("a"), []byte("b")})
		if err != nil {
			t.Fatal(err)
		}
		dec, state, err := c.DecryptBatch(ctx, "s1", []string{string(enc[1].Value), string(enc[0].Value)})
		if err != nil || state != KeyFound {
			t.Fatal(state, err)
		}
		if string(dec[0].Value) != "b" || string(dec[1].Value) != "a" {
			t.Fatalf("order lost: %q %q", dec[0].Value, dec[1].Value)
		}
		if _, err := c.EncryptBatch(ctx, "s1", nil); err == nil {
			t.Fatal("an empty batch must be refused before any call")
		}
	})
}

func TestErasureLedgerPages(t *testing.T) {
	clients(t, func(t *testing.T, c Client, _ *Memory) {
		ctx := context.Background()
		for _, s := range []string{"a", "b", "c"} {
			_ = c.DestroyKey(ctx, s)
		}
		p1, err := c.ListErasures(ctx, 0, 2)
		if err != nil || strings.Join(p1.SubjectIDs, ",") != "a,b" || p1.Next != 2 {
			t.Fatalf("page 1: %+v %v", p1, err)
		}
		p2, _ := c.ListErasures(ctx, p1.Next, 2)
		if strings.Join(p2.SubjectIDs, ",") != "c" || p2.Next != 3 {
			t.Fatalf("page 2: %+v", p2)
		}
		if _, err := c.ListErasures(ctx, -1, 1); err == nil {
			t.Fatal("a negative cursor must be refused")
		}
	})
}

func TestHMACIsDeterministicAndPinsVersions(t *testing.T) {
	clients(t, func(t *testing.T, c Client, m *Memory) {
		ctx := context.Background()
		if _, err := c.HMAC(ctx, "idx", []byte("x"), 0); !errors.As(err, new(*IndexKeyNotFoundError)) {
			t.Fatalf("want IndexKeyNotFoundError before ensure, got %v", err)
		}
		_ = c.EnsureIndexKey(ctx, "idx")
		h1, _ := c.HMAC(ctx, "idx", []byte("alice"), 0)
		h2, _ := c.HMAC(ctx, "idx", []byte("alice"), 1)
		if h1 != h2 || !strings.HasPrefix(h1, "vault:v1:") {
			t.Fatalf("latest and v1 must agree before rotation: %s %s", h1, h2)
		}
		batch, err := c.HMACBatch(ctx, "idx", [][]byte{[]byte("alice")}, 1)
		if err != nil || string(batch[0].Value) != h1 {
			t.Fatalf("batch must equal single: %v %v", batch, err)
		}
		_ = m.RotateIndexKey("idx")
		if v, _ := c.IndexKeyVersion(ctx, "idx"); v != 2 {
			t.Fatalf("want version 2, got %d", v)
		}
		pinned, _ := c.HMAC(ctx, "idx", []byte("alice"), 1)
		latest, _ := c.HMAC(ctx, "idx", []byte("alice"), 0)
		if pinned != h1 || latest == h1 || !strings.HasPrefix(latest, "vault:v2:") {
			t.Fatalf("pinned %s latest %s", pinned, latest)
		}
		_, err = c.HMAC(ctx, "idx", []byte("alice"), 9)
		if !IsUnavailable(err) {
			t.Fatalf("a version that doesn't exist is the facade's 400: %v", err)
		}
	})
}

func TestNewHTTPClientRefusesARelativeURL(t *testing.T) {
	for _, u := range []string{"", "kms.local", "/v1"} {
		if _, err := NewHTTPClient(u, nil); err == nil {
			t.Errorf("%q must be refused", u)
		}
	}
}

func TestUnreachableFacadeIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, _ := NewHTTPClient(url, nil)
	err := c.EnsureKey(context.Background(), "s1")
	if err == nil || !IsUnavailable(err) {
		t.Fatalf("a refused connection must be unavailable: %v", err)
	}
}
