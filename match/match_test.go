package match

import (
	"strings"
	"testing"
)

// c joins its parts, where an int is a code point: the vectors' own
// "adjacent tokens concatenate" notation, with no escapes in the source.
func c(parts ...any) string {
	var b strings.Builder
	for _, p := range parts {
		switch v := p.(type) {
		case string:
			b.WriteString(v)
		case int:
			b.WriteRune(rune(v))
		}
	}
	return b.String()
}

// The schema 3.1.1 test vectors (design-notes.md, "v3.1.1"), every row.
var vectors = []struct {
	normalize, in, want string
}{
	{CaseFold, "  Ada@Example.COM  ", "ada@example.com"},
	{CaseFold, c("Stra", 0x00DF, "e"), c("stra", 0x00DF, "e")},
	{CaseFold, c(0xFF21, 0xFF22, "c"), "abc"},
	{CaseFold, c(0x0130, "stanbul"), c(0x0130, "stanbul")},
	{CaseFold, "I", "i"},
	{CaseFold, c(0x1E9E), c(0x00DF)},
	{CaseFold, c(0x03A3, 0x0391, 0x03A3), c(0x03C3, 0x03B1, 0x03C3)},
	{CaseFold, c(0x10400), c(0x10428)},
	{CaseFold, c("cafe", 0x0301), c("caf", 0x00E9)},
	{CaseFold, c(0x00A0, "x", 0x3000), "x"},
	{Email, "Ada.Lovelace@Example.org ", "ada.lovelace@example.org"},
	{PersonName, c("  Zo", 0x00EB, "   M", 0x00FC, "ller "), "zoe muller"},
	{PersonName, c("Jos", 0x00E9, 0x0009, "Garc", 0x00ED, "a"), "jose garcia"},
	{PersonName, c(0xD55C, 0xAE00), c(0xD55C, 0xAE00)},
	{PersonName, "O'Brien-Smith", "o'brien-smith"},
	{PersonName, c(0x1E9E, "tra", 0x00DF, "e"), c(0x00DF, "tra", 0x00DF, "e")},
	{Phone, " +44 (0)20 7946-0958 ", "+4402079460958"},
	{Phone, "020 7946 0958", "02079460958"},
	{Phone, "00 44 20 7946 0958", "00442079460958"},
	{Phone, "1+2+3", "123"},
	{Phone, c(0xFF11, 0xFF12, "3"), "3"},
	{Phone, c(0x0661, 0x0662, "3"), "3"},
	{Phone, c(0xFF0B, "44 20"), "4420"},
	{None, "  Ada ", "  Ada "},
}

func TestSchemaVectors(t *testing.T) {
	if len(vectors) != 24 {
		t.Fatalf("the design notes pin 24 vectors, this table has %d", len(vectors))
	}
	for i, v := range vectors {
		got, err := Normalize(v.normalize, v.in)
		if err != nil || got != v.want {
			t.Errorf("vector %d (%s %q): got %q (%+q), want %q (%+q) %v", i, v.normalize, v.in, got, got, v.want, v.want, err)
		}
	}
	if got, _ := Normalize("", "ADA"); got != "ada" {
		t.Errorf("an absent normalize is caseFold, got %q", got)
	}
	if _, err := Normalize("soundex", "x"); err == nil {
		t.Error("an unknown normalizer must be refused")
	}
}

func TestPrefixesAreCodePoints(t *testing.T) {
	got := strings.Join(Prefixes("alice", 3), ",")
	if got != "ali,alic,alice" {
		t.Errorf("got %s", got)
	}
	if len(Prefixes("al", 3)) != 0 {
		t.Error("shorter than the minimum: no prefixes")
	}
	emoji := c("a", 0x1F600, "b")
	p := Prefixes(emoji, 2)
	if len(p) != 2 || p[0] != c("a", 0x1F600) || p[1] != emoji || CodePointLength(emoji) != 3 {
		t.Errorf("prefixes must cut at code points, never inside one: %q", p)
	}
}

func TestEscapeLike(t *testing.T) {
	bs := string(rune(92))
	cases := map[string]string{"50%": "50" + bs + "%", "a_b": "a" + bs + "_b", "a" + bs + "b": "a" + bs + bs + "b"}
	for in, want := range cases {
		if got := EscapeLike(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
