// Package authverify lets a --cqrsRole=secondary node verify bearer tokens
// it cannot verify itself — the fix for F-13.
//
// PocketBase verifies an auth token with the HMAC key
// record.TokenKey() + collection.AuthToken.Secret, both of which live only
// in each node's own data.db — which is deliberately never replicated (only
// events.db is; see docs/reference/cli.md's "Multi-node" section). So a
// token minted by the master is unverifiable on a secondary, and vice
// versa: no --cqrsForwardAuth setting alone makes a secondary both read-
// and write-capable for a logged-in user. Syncing secrets does not fix
// this either — the per-record TokenKey
// would still need a live master read per check (see the planning record's
// multi-node auth design analysis, shapes B/B′).
//
// What this package does instead (shape C′, cached remote verification):
// the master exposes a verify endpoint that runs the exact same
// FindAuthRecordByToken check its own middleware applies to direct
// requests; a secondary calls it on first sight of a token, materializes a
// local core.Record from the answer, and caches the verdict for a bounded
// TTL (never past the token's own exp claim). Cache hits serve
// authenticated local reads with no master round-trip; a definitive "no"
// evicts; an unreachable master fails closed unless an operator opted into
// a bounded grace window. The ops routes' superuser gate deliberately does
// NOT use the cache — an admin revocation must bite immediately, so it
// re-verifies against the master on every request (plain shape C).
package authverify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ErrInvalidToken is a definitive "no" from the master: the token failed
// the same check the master applies to its own direct requests. Callers
// treat it exactly like a local verification failure.
var ErrInvalidToken = errors.New("authverify: token rejected by master")

// ErrUnreachable means no verdict could be obtained at all — the master
// could not be reached, or answered with something other than a verdict.
// Deliberately distinct from ErrInvalidToken: the caller must fail closed
// (503, retryable) rather than tell the client its token is bad (401,
// which would send users to a login flow that cannot work either while the
// master is down).
var ErrUnreachable = errors.New("authverify: master unreachable, no verdict")

// Verdict is the master's answer for a valid token: which auth collection
// the token's record belongs to, and the record itself as the master would
// serialize it for its own holder (PublicExport — tokenKey and password
// are never included).
type Verdict struct {
	CollectionName string          `json:"collectionName"`
	CollectionID   string          `json:"collectionId"`
	Record         json.RawMessage `json:"record"`
}

// verifyPath is where RegisterEndpoint binds on every node.
const verifyPath = "/api/cqrs/auth/verify"

// maxVerdictBytes bounds how much of a verify response a client reads —
// a record export is small; anything bigger is not a verdict.
const maxVerdictBytes = 1 << 20

// Client calls a master's verify endpoint.
type Client struct {
	verifyURL string
	http      *http.Client
}

// NewClient returns a client for the master at base (the same base URL
// --cqrsMasterAddr names). The client carries its own short timeout — a
// verification check must answer fast or count as unreachable, it must
// never hold a request open the way a proxied command legitimately may.
func NewClient(base *url.URL) *Client {
	return &Client{
		verifyURL: base.JoinPath(verifyPath).String(),
		http:      &http.Client{Timeout: 5 * time.Second},
	}
}

// Verify asks the master for a verdict on token.
//
// Returns the verdict, ErrInvalidToken on a definitive rejection, or an
// error wrapping ErrUnreachable for everything that is not an answer
// (network failure, or any status other than the two the endpoint speaks).
func (c *Client) Verify(ctx context.Context, token string) (*Verdict, error) {
	body, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.verifyURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var v Verdict
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxVerdictBytes)).Decode(&v); err != nil {
			return nil, fmt.Errorf("%w: malformed verdict: %v", ErrUnreachable, err)
		}
		if v.CollectionName == "" || len(v.Record) == 0 {
			return nil, fmt.Errorf("%w: incomplete verdict", ErrUnreachable)
		}
		return &v, nil
	case http.StatusUnauthorized:
		return nil, ErrInvalidToken
	default:
		return nil, fmt.Errorf("%w: master answered %d", ErrUnreachable, resp.StatusCode)
	}
}
