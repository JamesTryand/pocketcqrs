package authverify

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/pocketbase/pocketbase/tools/security"
	"golang.org/x/sync/singleflight"
)

// Verifier answers "is this token valid, and for whom" on a node that
// cannot answer locally. VerifyCached is shape C′ (bounded-TTL cache,
// optional grace); VerifyFresh is shape C (a master round-trip every time),
// for the callers where a stale "yes" is not acceptable — the ops routes'
// superuser gate above all.
type Verifier struct {
	client *Client
	cache  *Cache
	ttl    time.Duration
	grace  time.Duration
	group  singleflight.Group

	// now is stubbed by tests; everything time-dependent goes through it.
	now func() time.Time
}

// New builds a Verifier against the master at base. ttl bounds how long a
// verdict is trusted without re-checking (always additionally capped by
// each token's own exp claim); grace is how far past expiry a stale
// verdict may still be served when the master is unreachable — 0 means
// fail closed the moment a verdict expires.
func New(base *url.URL, cache *Cache, ttl, grace time.Duration) *Verifier {
	return &Verifier{
		client: NewClient(base),
		cache:  cache,
		ttl:    ttl,
		grace:  grace,
		now:    time.Now,
	}
}

// tokenExp reads the token's own exp claim, rejecting a token that is
// malformed or already expired/not-yet-valid without any network cost.
// The claims are NOT signature-checked here — that is exactly what only
// the master can do — so exp is only ever used to SHORTEN trust (a forged
// exp still fails the master's signature check before anything is cached).
func tokenExp(token string) (time.Time, error) {
	claims, err := security.ParseUnverifiedJWT(token) // validates exp, iat, nbf
	if err != nil {
		return time.Time{}, err
	}
	exp, ok := claims["exp"].(float64)
	if !ok {
		return time.Time{}, errors.New("token has no exp claim")
	}
	return time.Unix(int64(exp), 0), nil
}

// VerifyCached returns a verdict for token, from cache when a live entry
// exists, otherwise from the master. Returns ErrInvalidToken for a
// definitive "no" (local malformed/expired, or rejected by the master —
// which also evicts any cached entry), or an error wrapping ErrUnreachable
// when no verdict could be had: master down, cache empty or past expiry,
// and past any grace window.
func (v *Verifier) VerifyCached(ctx context.Context, token string) (*Verdict, error) {
	exp, err := tokenExp(token)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	hash := HashToken(token)

	entry, err := v.cache.Lookup(ctx, hash)
	if err != nil {
		// a broken cache must degrade to shape C, not to an outage
		entry = nil
	}
	now := v.now()
	if entry != nil && now.Before(entry.ExpiresAt) {
		return &entry.Verdict, nil
	}

	verdict, err := v.remoteVerify(ctx, token, hash, exp)
	if err != nil && errors.Is(err, ErrUnreachable) && entry != nil && v.grace > 0 {
		// the operator opted into serving stale verdicts during an outage,
		// bounded twice over: by the grace window, and by the token's own exp
		graceUntil := entry.ExpiresAt.Add(v.grace)
		if entry.TokenExp.Before(graceUntil) {
			graceUntil = entry.TokenExp
		}
		if now.Before(graceUntil) {
			return &entry.Verdict, nil
		}
	}
	return verdict, err
}

// VerifyFresh always asks the master — no cache read, no grace. The
// verdict is still written through to the cache (a fresh answer is the
// best possible cache content), and a definitive rejection still evicts.
func (v *Verifier) VerifyFresh(ctx context.Context, token string) (*Verdict, error) {
	exp, err := tokenExp(token)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	return v.remoteVerify(ctx, token, HashToken(token), exp)
}

// remoteVerify asks the master once per in-flight token (concurrent
// requests bearing the same token share one round-trip), maintaining the
// cache from the answer.
func (v *Verifier) remoteVerify(ctx context.Context, token, hash string, exp time.Time) (*Verdict, error) {
	res, err, _ := v.group.Do(hash, func() (any, error) {
		verdict, err := v.client.Verify(ctx, token)
		if err != nil {
			if errors.Is(err, ErrInvalidToken) && v.cache != nil {
				_ = v.cache.Delete(ctx, hash)
			}
			return nil, err
		}
		if v.cache != nil {
			now := v.now()
			expiresAt := now.Add(v.ttl)
			if exp.Before(expiresAt) {
				expiresAt = exp // a verdict must never outlive its token
			}
			// best-effort: a failed save costs a future round-trip, not the answer
			_ = v.cache.Save(ctx, hash, verdict, exp, now, expiresAt)
		}
		return verdict, nil
	})
	if err != nil {
		return nil, err
	}
	return res.(*Verdict), nil
}
