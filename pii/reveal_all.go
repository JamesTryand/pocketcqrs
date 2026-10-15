package pii

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jamestryand/pocketcqrs/kms"
)

// RevealAll returns v, a decoded JSON value, with every personal-data
// envelope ({"$pii":{"s":subject,"c":ciphertext}}) at any depth replaced by
// its plaintext, decoded. A redacted value ("c": null) or an erased
// subject's is nil. For checking what a decider or projection produced
// against a scenario's plain values (schema verify's Go harness); the
// query route reveals for callers through the cache instead.
func RevealAll(ctx context.Context, c kms.Client, v any) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		if env, ok := x[envelopeKey].(map[string]any); ok && len(x) == 1 {
			subject, _ := env["s"].(string)
			cipher, _ := env["c"].(string)
			if subject == "" || cipher == "" {
				return nil, nil
			}
			plain, state, err := c.Decrypt(ctx, subject, cipher)
			if err != nil {
				return nil, fmt.Errorf("pii: reveal %s: %w", subject, err)
			}
			if state == kms.KeyDestroyed {
				return nil, nil
			}
			var out any
			if err := json.Unmarshal(plain, &out); err != nil {
				return nil, fmt.Errorf("pii: reveal %s: %w", subject, err)
			}
			return out, nil
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			r, err := RevealAll(ctx, c, e)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			r, err := RevealAll(ctx, c, e)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}
