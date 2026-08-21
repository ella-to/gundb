package gundb

import (
	"cmp"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// jsonNumber renders f the way JSON.stringify does (ECMAScript
// Number::toString): fixed-point for 1e-6 <= |f| < 1e21, otherwise
// exponent form with no leading zeros ("1e-7", "1.5e+22").
func jsonNumber(f float64) string {
	if f == 0 {
		return "0"
	}
	if abs := math.Abs(f); abs < 1e-6 || abs >= 1e21 {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		mant, exp, _ := strings.Cut(s, "e")
		sign := exp[0]
		exp = strings.TrimLeft(exp[1:], "0")
		if exp == "" {
			exp = "0"
		}
		return mant + "e" + string(sign) + exp
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// jsQuote quotes s exactly like JSON.stringify: only ", \ and control
// characters are escaped. (encoding/json additionally escapes <, >, &,
// U+2028 and U+2029, which would break HAM's lexical tie-break.)
func jsQuote(s string) string {
	const hex = "0123456789abcdef"
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hex[r>>4])
				b.WriteByte(hex[r&0xF])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// jsCompare compares two strings the way JavaScript's < operator does: by
// UTF-16 code units rather than by UTF-8 bytes. The two orders differ for
// characters above U+FFFF versus U+E000..U+FFFF.
func jsCompare(a, b string) int {
	for len(a) > 0 && len(b) > 0 {
		ra, na := utf8.DecodeRuneInString(a)
		rb, nb := utf8.DecodeRuneInString(b)
		if ra != rb {
			if c := cmp.Compare(utf16Unit(ra, 0), utf16Unit(rb, 0)); c != 0 {
				return c
			}
			return cmp.Compare(utf16Unit(ra, 1), utf16Unit(rb, 1))
		}
		a, b = a[na:], b[nb:]
	}
	return cmp.Compare(len(a), len(b))
}

// utf16Unit returns the i-th (0 or 1) UTF-16 code unit of r.
func utf16Unit(r rune, i int) rune {
	if r < 0x10000 {
		return r
	}
	r -= 0x10000
	if i == 0 {
		return 0xD800 + (r >> 10)
	}
	return 0xDC00 + (r & 0x3FF)
}
