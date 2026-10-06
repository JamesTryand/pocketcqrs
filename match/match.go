// Package match is the normalization behind readModel match filters
// (schema 3.1.0, pinned exactly by 3.1.1's design notes). It must produce
// the same bytes as every other implementation (dotnetcqrs's
// MatchNormalizer is the reference that generated the vectors): a hashed
// search index only matches when writer and reader normalize identically,
// and a mismatch shows up as silently missing results, not an error. So
// these are the pinned rules, not Go's nearest equivalents — in particular
// not cases.Fold (full folding turns ß into ss; the rules don't).
package match

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Normalizers, as the schema names them. "" is the default, caseFold.
const (
	None       = "none"
	CaseFold   = "caseFold"
	Email      = "email"
	Phone      = "phone"
	PersonName = "personName"
)

// Modes.
const (
	Exact    = "exact"
	Prefix   = "prefix"
	Contains = "contains"
)

// Normalize applies one pinned rule.
func Normalize(normalize, value string) (string, error) {
	switch normalize {
	case None:
		return value, nil
	case "", CaseFold, Email:
		return caseFold(value), nil
	case PersonName:
		return personName(value), nil
	case Phone:
		return phone(value), nil
	}
	return "", fmt.Errorf("match: unknown normalize %q", normalize)
}

func isWhite(r rune) bool { return unicode.Is(unicode.White_Space, r) }

// caseFold: NFKC, then each code point's one-to-one invariant lowercase
// (U+0130 left as is, following .NET's invariant casing), then trim
// White_Space.
func caseFold(s string) string {
	s = norm.NFKC.String(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r != 0x0130 {
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return strings.TrimFunc(b.String(), isWhite)
}

// personName: caseFold, NFD, drop non-spacing marks (Mn), collapse each
// White_Space run to one space and trim, then NFC.
func personName(s string) string {
	s = norm.NFD.String(caseFold(s))
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Mn, r):
			continue
		case isWhite(r):
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	return norm.NFC.String(b.String())
}

// phone: trim White_Space; a leading ASCII '+' is kept; then every ASCII
// digit in order; everything else dropped. No NFKC, no country inference.
func phone(s string) string {
	s = strings.TrimFunc(s, isWhite)
	var b strings.Builder
	if strings.HasPrefix(s, "+") {
		b.WriteByte('+')
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= '0' && c <= '9' {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// CodePointLength counts Unicode code points (Go runes), the unit
// minPrefixLength is measured in — never bytes, never UTF-16 units.
func CodePointLength(s string) int { return utf8.RuneCountInString(s) }

// Prefixes is every prefix of value from minLength code points up to the
// whole value, shortest first, cut at code point boundaries (pinned: a
// hashed prefix index stores one hash per prefix; a search hashes the whole
// term). Empty when value is shorter than minLength.
func Prefixes(value string, minLength int) []string {
	if minLength < 1 {
		minLength = 1
	}
	var out []string
	n := 0
	for i := range value {
		if n >= minLength {
			out = append(out, value[:i])
		}
		n++
	}
	if n >= minLength {
		out = append(out, value)
	}
	return out
}

// EscapeLike escapes a term for a LIKE pattern with ESCAPE '\'.
func EscapeLike(term string) string {
	bs := string(rune(92))
	r := strings.NewReplacer(bs, bs+bs, "%", bs+"%", "_", bs+"_")
	return r.Replace(term)
}
