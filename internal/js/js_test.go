package js

import (
	"cmp"
	"encoding/json"
	"slices"
	"testing"
	"unicode/utf16"
)

var strs = []string{
	"", "a", "ab", "abc", "chat/2026", "chat/2027", "é", "é", " ",
	"", "￿", "😀", "😀a", "a😀", "a", "\"q\"", `back\slash`,
	"tab\there", "nl\n", "\x00\x1f", "<&>", "\xff", "a\xffb", "ok\xc3",
}

func TestQuote(t *testing.T) {
	for _, s := range strs {
		got := Quote(s)
		if app := string(AppendQuote([]byte("x"), s)); app != "x"+got {
			t.Errorf("AppendQuote(%q) = %s, Quote = %s", s, app, got)
		}
		// JSON.stringify's output is valid JSON that parses back to the
		// string (with invalid UTF-8 replaced) and escapes nothing above
		// the control range besides " and \.
		var back string
		if err := json.Unmarshal([]byte(got), &back); err != nil {
			t.Fatalf("Quote(%q) = %s: %v", s, got, err)
		}
		if want := string([]rune(s)); back != want {
			t.Errorf("Quote(%q) round trips to %q", s, back)
		}
	}
	for s, want := range map[string]string{
		"<&> ":       `"<&>` + " " + `"`,
		"a\"b\\c":    `"a\"b\\c"`,
		"\b\f\n\r\t": `"\b\f\n\r\t"`,
		"\x01\x1f":   `"\u0001\u001f"`,
		"a\xffb":     `"a` + "�" + `b"`,
	} {
		if got := Quote(s); got != want {
			t.Errorf("Quote(%q) = %s, want %s", s, got, want)
		}
	}
}

func TestCompareIsUTF16Order(t *testing.T) {
	for _, a := range strs {
		for _, b := range strs {
			want := slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b)))
			if got := Compare(a, b); cmp.Compare(got, 0) != want {
				t.Errorf("Compare(%q, %q) = %d, want %d", a, b, got, want)
			}
		}
	}
}

// TestRestringifyRejectsWhatJSONParseRejects covers text that starts like
// JSON but isn't: SEA must hand such strings back as strings.
func TestRestringifyRejectsWhatJSONParseRejects(t *testing.T) {
	for _, s := range []string{"1Ab+cd=", "12/xy", "1e5xyz", "1 2", "truex", "null!", `"a"b`, `{"a":1}x`, "[1]]"} {
		if out, err := Restringify([]byte(s)); err == nil {
			t.Errorf("Restringify(%q) = %s, want an error", s, out)
		}
	}
	for s, want := range map[string]string{" 1 ": "1", "\n{\"a\": 1}\t": `{"a":1}`, `"x"`: `"x"`, "1e5": "100000"} {
		if out, err := Restringify([]byte(s)); err != nil || string(out) != want {
			t.Errorf("Restringify(%q) = %s, %v; want %s", s, out, err, want)
		}
	}
}
