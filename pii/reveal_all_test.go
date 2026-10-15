package pii

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jamestryand/pocketcqrs/kms"
)

// RevealAll opens every envelope in a decoded JSON value, at any depth, and
// leaves everything else as it was. An erased subject's value is nil.
func TestRevealAll(t *testing.T) {
	ctx := context.Background()
	c := kms.NewMemory()
	seal := func(subject string, v any) any {
		if err := c.EnsureKey(ctx, subject); err != nil {
			t.Fatal(err)
		}
		plain, _ := json.Marshal(v)
		cipher, err := c.Encrypt(ctx, subject, plain)
		if err != nil {
			t.Fatal(err)
		}
		return map[string]any{"$pii": map[string]any{"s": subject, "c": cipher}}
	}
	in := map[string]any{
		"name":  seal("s1", "Ada"),
		"plain": 7.0,
		"lines": []any{map[string]any{"note": seal("s1", map[string]any{"x": 1.0})}, "y"},
		"gone":  seal("s2", "Bob"),
	}
	if err := c.DestroyKey(ctx, "s2"); err != nil {
		t.Fatal(err)
	}
	out, err := RevealAll(ctx, c, in)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(out)
	want := `{"gone":null,"lines":[{"note":{"x":1}},"y"],"name":"Ada","plain":7}`
	if string(got) != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}
