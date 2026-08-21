package gundb

import (
	"encoding/json"
	"testing"
)

func TestValueJSON(t *testing.T) {
	cases := []struct {
		v    Value
		want string
	}{
		{Null{}, `null`},
		{Bool(true), `true`},
		{Number(30), `30`},
		{Number(1.5), `1.5`},
		{Number(1e21), `1e+21`},
		{Number(1e-7), `1e-7`},
		{String("a\"<b>\n"), `"a\"<b>\n"`},
		{Link{Soul: "mark/boss"}, `{"#":"mark/boss"}`},
	}
	for _, c := range cases {
		if got := lexical(c.v); got != c.want {
			t.Errorf("lexical(%#v) = %s, want %s", c.v, got, c.want)
		}
		back, err := decodeValue(json.RawMessage(c.want))
		if err != nil || back != c.v {
			t.Errorf("decodeValue(%s) = %#v, %v", c.want, back, err)
		}
	}
}

func TestDecodeValueRejects(t *testing.T) {
	for _, raw := range []string{`[1]`, `{"a":1}`, `{"#":"a","b":1}`, `{"#":1}`, ``} {
		if _, err := decodeValue(json.RawMessage(raw)); err == nil {
			t.Errorf("decodeValue(%s) should fail", raw)
		}
	}
}

func TestJSCompareUsesUTF16Order(t *testing.T) {
	// U+1F600 is "😀" in UTF-16, which JS sorts before U+FFFD,
	// while UTF-8 byte order says the opposite.
	if jsCompare("😀", "�") >= 0 {
		t.Fatal("want 😀 < U+FFFD as in JavaScript")
	}
	if jsCompare("a", "ab") >= 0 || jsCompare("b", "a") <= 0 || jsCompare("x", "x") != 0 {
		t.Fatal("basic ordering broken")
	}
}
