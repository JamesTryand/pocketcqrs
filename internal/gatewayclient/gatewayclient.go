// Package gatewayclient is a small HTTP client for the pocketcqrs command
// gateway (POST /api/cqrs/{aggregate}/{id}/{command}), for use by
// out-of-process components in this repo that need to dispatch a follow-up
// command.
//
// This is an ordinary authenticated internal call, not subject to the
// outbound package's SSRF-motivated allow-list — that package is for calls
// to arbitrary third parties, not to a deployment's own trusted gateway.
//
// The gateway's HTTP route has no way for a caller to supply the
// causationId/correlationId/commandId/provenance metadata that
// reactors.Dispatch uses for its in-process redelivery dedup
// (reactors.go's CommandApplied check) — actorMeta in gateway.go derives
// actor/actorCollection from the authenticated caller only. An out-of-process
// dispatcher gets the equivalent guarantee a different way: the gateway's
// Idempotency store is wired unconditionally at boot (not behind a flag), so
// a caller-supplied Idempotency-Key header on a retried POST with the same
// key and body replays the original response instead of double-applying.
// Client.Dispatch always sets one, derived deterministically so a redelivered
// source event produces the same key.
package gatewayclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jamestryand/pocketcqrs/events"
)

// Client dispatches commands to one pocketcqrs deployment's gateway.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New builds a Client. baseURL is the deployment's root (e.g.
// "http://localhost:8090", no trailing slash required); token is sent as
// "Authorization: Bearer <token>" on every request — minting it (a superuser
// token or a dedicated service-account record) is an operator decision, out
// of scope for this package.
func New(baseURL, token string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: timeout},
	}
}

// Command is one command to dispatch.
type Command struct {
	Aggregate string
	ID        string
	Name      string
	Payload   json.RawMessage

	// IdempotencyKeyParts, hashed together, derive the Idempotency-Key
	// header deterministically: the same parts (typically the dispatching
	// component's name and the causing event's id) must always be supplied
	// for a given logical dispatch, so a redelivery of the same source
	// event reuses the same key and gets the original response replayed
	// rather than double-applying.
	IdempotencyKeyParts []string

	// CausationID and CorrelationID, when non-empty, are sent as the
	// Causation-Id/Correlation-Id request headers — honored by the gateway
	// only for a caller its ExternalCallerCollection recognizes (see
	// gateway.actorMeta's doc comment); ignored otherwise. Empty means
	// "don't send the header", not "send it empty".
	CausationID   string
	CorrelationID string
}

// Result is a successful dispatch's outcome.
type Result struct {
	Status int
	Events []events.Event
}

// ErrStatus is returned when the gateway answers with a non-2xx status.
// Status carries the exact code (400/401/404/409/503/504 per the gateway's
// documented contract) so callers can distinguish a domain rejection (400),
// a concurrency conflict (409), maintenance mode (503) or a timeout (504)
// without string-matching Body.
type ErrStatus struct {
	Status int
	Body   string
}

func (e *ErrStatus) Error() string {
	return fmt.Sprintf("gatewayclient: gateway returned %d: %s", e.Status, e.Body)
}

// Dispatch POSTs cmd to the gateway and returns the appended events.
func (c *Client) Dispatch(ctx context.Context, cmd Command) (*Result, error) {
	payload := cmd.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}

	url := fmt.Sprintf("%s/api/cqrs/%s/%s/%s", c.baseURL, cmd.Aggregate, cmd.ID, cmd.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("gatewayclient: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Idempotency-Key", idempotencyKey(cmd.IdempotencyKeyParts))
	if cmd.CausationID != "" {
		req.Header.Set("Causation-Id", cmd.CausationID)
	}
	if cmd.CorrelationID != "" {
		req.Header.Set("Correlation-Id", cmd.CorrelationID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gatewayclient: %s %s: %w", http.MethodPost, url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gatewayclient: reading response from %s: %w", url, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &ErrStatus{Status: resp.StatusCode, Body: string(body)}
	}

	var decoded struct {
		Events []events.Event `json:"events"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("gatewayclient: decoding response from %s: %w", url, err)
	}
	return &Result{Status: resp.StatusCode, Events: decoded.Events}, nil
}

// idempotencyKey hashes parts into a deterministic header value. Order
// matters, same as idempotency.Hash on the gateway side: callers must supply
// the same parts in the same order for the same logical dispatch every time.
func idempotencyKey(parts []string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "extcall-" + hex.EncodeToString(h.Sum(nil))
}
