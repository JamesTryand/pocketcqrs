package kms

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// EnvFacadeURL names the facade's base URL (e.g.
// https://kms.bureau.tryand.uk:8080/), matching the facade's own kms.env
// naming and dotnetcqrs's generated hosts. A host that stores personal data
// and has no facade URL must refuse to start, never fall back to Memory.
const EnvFacadeURL = "KMS_FACADE_URL"

// HTTPClient is the facade over HTTPS. It sends no credentials of its own:
// caller-to-facade authentication is still an open item on the facade side,
// so Client (the *http.Client) is the drop-in point for it — TLS config,
// timeouts and any auth transport are the caller's.
type HTTPClient struct {
	base   *url.URL
	client *http.Client
}

// NewHTTPClient parses baseURL (absolute; a trailing slash is added if
// missing, so "v1/..." resolves under it). client nil means
// http.DefaultClient.
func NewHTTPClient(baseURL string, client *http.Client) (*HTTPClient, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/")
	if err != nil || !u.IsAbs() || u.Host == "" {
		return nil, fmt.Errorf("kms: facade URL %q must be an absolute http(s) URL", baseURL)
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPClient{base: u, client: client}, nil
}

var _ Client = (*HTTPClient)(nil)

func subjectPath(id, op string) string { return "v1/subjects/" + url.PathEscape(id) + "/" + op }
func indexKeyPath(name string) string  { return "v1/index-keys/" + url.PathEscape(name) }

func b64(p []byte) string { return base64.StdEncoding.EncodeToString(p) }

// do sends one request and returns the status and body. Transport failures
// come back as-is (net errors, deadline), so IsUnavailable recognises them.
func (c *HTTPClient) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	ref, err := url.Parse(path)
	if err != nil {
		return 0, nil, err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.ResolveReference(ref).String(), rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, b, nil
}

func ok(status int) bool { return status >= 200 && status < 300 }

func decode(op string, body []byte, v any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return &ProtocolError{Msg: op + " response body was empty"}
	}
	if err := json.Unmarshal(body, v); err != nil {
		return &ProtocolError{Msg: op + " response body was malformed", Err: err}
	}
	return nil
}

func unb64(op, s string) ([]byte, error) {
	p, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, &ProtocolError{Msg: op + " returned invalid base64", Err: err}
	}
	return p, nil
}

func (c *HTTPClient) EnsureKey(ctx context.Context, subjectID string) error {
	status, _, err := c.do(ctx, http.MethodPut, subjectPath(subjectID, "key"), nil)
	switch {
	case err != nil:
		return err
	case status == http.StatusConflict:
		return &SubjectErasedError{SubjectID: subjectID}
	case !ok(status):
		return &StatusError{Op: "ensure key", Status: status}
	}
	return nil
}

func (c *HTTPClient) Encrypt(ctx context.Context, subjectID string, plaintext []byte) (string, error) {
	status, body, err := c.do(ctx, http.MethodPost, subjectPath(subjectID, "encrypt"),
		map[string]string{"plaintext": b64(plaintext)})
	switch {
	case err != nil:
		return "", err
	case status == http.StatusNotFound:
		return "", &KeyNotFoundError{SubjectID: subjectID}
	case !ok(status):
		return "", &StatusError{Op: "encrypt", Status: status}
	}
	var r struct {
		Ciphertext string `json:"ciphertext"`
	}
	if err := decode("encrypt", body, &r); err != nil {
		return "", err
	}
	return r.Ciphertext, nil
}

func (c *HTTPClient) Decrypt(ctx context.Context, subjectID, ciphertext string) ([]byte, KeyState, error) {
	status, body, err := c.do(ctx, http.MethodPost, subjectPath(subjectID, "decrypt"),
		map[string]string{"ciphertext": ciphertext})
	switch {
	case err != nil:
		return nil, KeyFound, err
	case status == http.StatusNotFound:
		return nil, KeyDestroyed, nil
	case !ok(status):
		return nil, KeyFound, &StatusError{Op: "decrypt", Status: status}
	}
	var r struct {
		Plaintext string `json:"plaintext"`
	}
	if err := decode("decrypt", body, &r); err != nil {
		return nil, KeyFound, err
	}
	p, err := unb64("decrypt", r.Plaintext)
	return p, KeyFound, err
}

type batchResult struct {
	Ciphertext *string `json:"ciphertext"`
	Plaintext  *string `json:"plaintext"`
	Hmac       *string `json:"hmac"`
	Error      *string `json:"error"`
}

// items converts a batch response, checking it answers every input — a
// short or long result list would silently misalign values with inputs.
func items(op string, want int, results []batchResult, value func(batchResult) (*string, bool)) ([]BatchItem, error) {
	if len(results) != want {
		return nil, &ProtocolError{Msg: fmt.Sprintf("%s returned %d results for %d inputs", op, len(results), want)}
	}
	out := make([]BatchItem, len(results))
	for i, r := range results {
		if r.Error != nil && *r.Error != "" {
			out[i] = BatchItem{Err: *r.Error}
			continue
		}
		v, isB64 := value(r)
		if v == nil {
			return nil, &ProtocolError{Msg: fmt.Sprintf("%s result %d has neither a value nor an error", op, i)}
		}
		if isB64 {
			p, err := unb64(op, *v)
			if err != nil {
				return nil, err
			}
			out[i] = BatchItem{Value: p}
		} else {
			out[i] = BatchItem{Value: []byte(*v)}
		}
	}
	return out, nil
}

func (c *HTTPClient) EncryptBatch(ctx context.Context, subjectID string, plaintexts [][]byte) ([]BatchItem, error) {
	if err := checkBatch(len(plaintexts), "plaintexts"); err != nil {
		return nil, err
	}
	in := make([]string, len(plaintexts))
	for i, p := range plaintexts {
		in[i] = b64(p)
	}
	status, body, err := c.do(ctx, http.MethodPost, subjectPath(subjectID, "encrypt-batch"),
		map[string]any{"plaintexts": in})
	switch {
	case err != nil:
		return nil, err
	case status == http.StatusNotFound:
		return nil, &KeyNotFoundError{SubjectID: subjectID}
	case !ok(status):
		return nil, &StatusError{Op: "encrypt-batch", Status: status}
	}
	var r struct {
		Results []batchResult `json:"results"`
	}
	if err := decode("encrypt-batch", body, &r); err != nil {
		return nil, err
	}
	return items("encrypt-batch", len(plaintexts), r.Results, func(b batchResult) (*string, bool) { return b.Ciphertext, false })
}

func (c *HTTPClient) DecryptBatch(ctx context.Context, subjectID string, ciphertexts []string) ([]BatchItem, KeyState, error) {
	if err := checkBatch(len(ciphertexts), "ciphertexts"); err != nil {
		return nil, KeyFound, err
	}
	status, body, err := c.do(ctx, http.MethodPost, subjectPath(subjectID, "decrypt-batch"),
		map[string]any{"ciphertexts": ciphertexts})
	switch {
	case err != nil:
		return nil, KeyFound, err
	case status == http.StatusNotFound:
		return nil, KeyDestroyed, nil
	case !ok(status):
		return nil, KeyFound, &StatusError{Op: "decrypt-batch", Status: status}
	}
	var r struct {
		Results []batchResult `json:"results"`
	}
	if err := decode("decrypt-batch", body, &r); err != nil {
		return nil, KeyFound, err
	}
	out, err := items("decrypt-batch", len(ciphertexts), r.Results, func(b batchResult) (*string, bool) { return b.Plaintext, true })
	return out, KeyFound, err
}

func (c *HTTPClient) DestroyKey(ctx context.Context, subjectID string) error {
	status, _, err := c.do(ctx, http.MethodDelete, subjectPath(subjectID, "key"), nil)
	switch {
	case err != nil:
		return err
	case !ok(status):
		return &StatusError{Op: "destroy key", Status: status}
	}
	return nil
}

func (c *HTTPClient) ListErasures(ctx context.Context, after int64, limit int) (ErasurePage, error) {
	if after < 0 {
		return ErasurePage{}, fmt.Errorf("kms: list erasures: after must be non-negative (got %d)", after)
	}
	if err := checkBatch(limit, "limit"); err != nil {
		return ErasurePage{}, err
	}
	status, body, err := c.do(ctx, http.MethodGet, fmt.Sprintf("v1/erasures?after=%d&limit=%d", after, limit), nil)
	switch {
	case err != nil:
		return ErasurePage{}, err
	case !ok(status):
		return ErasurePage{}, &StatusError{Op: "list erasures", Status: status}
	}
	var r struct {
		SubjectIDs *[]string `json:"subjectIds"`
		Next       int64     `json:"next"`
	}
	if err := decode("erasures", body, &r); err != nil {
		return ErasurePage{}, err
	}
	if r.SubjectIDs == nil {
		return ErasurePage{}, &ProtocolError{Msg: "erasures response has no subjectIds"}
	}
	return ErasurePage{SubjectIDs: *r.SubjectIDs, Next: r.Next}, nil
}

func (c *HTTPClient) EnsureIndexKey(ctx context.Context, name string) error {
	status, _, err := c.do(ctx, http.MethodPut, indexKeyPath(name), nil)
	switch {
	case err != nil:
		return err
	case !ok(status):
		return &StatusError{Op: "ensure index key", Status: status}
	}
	return nil
}

func (c *HTTPClient) IndexKeyVersion(ctx context.Context, name string) (int, error) {
	status, body, err := c.do(ctx, http.MethodGet, indexKeyPath(name), nil)
	switch {
	case err != nil:
		return 0, err
	case status == http.StatusNotFound:
		return 0, &IndexKeyNotFoundError{Name: name}
	case !ok(status):
		return 0, &StatusError{Op: "index key version", Status: status}
	}
	var r struct {
		LatestVersion int `json:"latestVersion"`
	}
	if err := decode("index-key", body, &r); err != nil {
		return 0, err
	}
	return r.LatestVersion, nil
}

// hmacBody omits keyVersion for "latest" rather than sending null or 0, per
// the contract.
func hmacBody(key string, val any, keyVersion int) map[string]any {
	m := map[string]any{key: val}
	if keyVersion != 0 {
		m["keyVersion"] = keyVersion
	}
	return m
}

func (c *HTTPClient) HMAC(ctx context.Context, name string, input []byte, keyVersion int) (string, error) {
	status, body, err := c.do(ctx, http.MethodPost, indexKeyPath(name)+"/hmac", hmacBody("input", b64(input), keyVersion))
	switch {
	case err != nil:
		return "", err
	case status == http.StatusNotFound:
		return "", &IndexKeyNotFoundError{Name: name}
	case !ok(status):
		return "", &StatusError{Op: "hmac", Status: status}
	}
	var r struct {
		Hmac string `json:"hmac"`
	}
	if err := decode("hmac", body, &r); err != nil {
		return "", err
	}
	return r.Hmac, nil
}

func (c *HTTPClient) HMACBatch(ctx context.Context, name string, inputs [][]byte, keyVersion int) ([]BatchItem, error) {
	if err := checkBatch(len(inputs), "inputs"); err != nil {
		return nil, err
	}
	in := make([]string, len(inputs))
	for i, p := range inputs {
		in[i] = b64(p)
	}
	status, body, err := c.do(ctx, http.MethodPost, indexKeyPath(name)+"/hmac-batch", hmacBody("inputs", in, keyVersion))
	switch {
	case err != nil:
		return nil, err
	case status == http.StatusNotFound:
		return nil, &IndexKeyNotFoundError{Name: name}
	case !ok(status):
		return nil, &StatusError{Op: "hmac-batch", Status: status}
	}
	var r struct {
		Results []batchResult `json:"results"`
	}
	if err := decode("hmac-batch", body, &r); err != nil {
		return nil, err
	}
	return items("hmac-batch", len(inputs), r.Results, func(b batchResult) (*string, bool) { return b.Hmac, false })
}
