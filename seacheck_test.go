package gundb

import (
	"encoding/json"
	"testing"

	"ella.to/gundb/sea"
)

// slowValue is what unsign does without signedValue.
func slowValue(s string) (Value, bool) {
	raw, ok := sea.FieldValue(s)
	if !ok {
		return nil, false
	}
	v, err := decodeValue(raw)
	return v, err == nil
}

// TestSignedValueMatchesFieldValue checks that the fast path reads every
// signed field exactly like sea.FieldValue, and declines what it can't.
func TestSignedValueMatchesFieldValue(t *testing.T) {
	pair, _ := sea.NewPair()
	values := []Value{
		String("Alice"), String(""), String(`with "quotes" and \ backslash`), String("tab\tnew\nline"),
		String("ünïcödé 🎉"), String(`,"~":"`), String(`"}`), String("<html> & stuff"),
		Number(0), Number(-1.5), Number(1e21), Number(1e-7), Bool(true), Bool(false), Null{},
		Link{Soul: "~pub/profile"},
	}
	for _, v := range values {
		stored, err := sea.SignField("~x.y", "f", json.RawMessage(lexical(v)), 1, pair)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := signedValue(stored)
		want, _ := slowValue(stored)
		if ok && got != want {
			t.Errorf("%s: fast %#v, slow %#v", stored, got, want)
		}
		if !ok {
			t.Errorf("%s: fast path declined a value SEA wrote", stored)
		}
	}
	for _, s := range []string{
		``, `{}`, `"plain"`, `{":":1}`, `{":":1,"~":"}`, `{":":1,"~":"a"b"}`, `{":":1,"~":"a\"b"}`,
		`{":":1 2,"~":"sig"}`, `{":":"a","~":"x","~":"sig"}`, `{":":{"x":1},"~":"sig"}`,
		`{":":1,"~":"sig","+":"cert"}`, `{"~":"sig",":":1}`, `{":":"\u0001","~":"sig"}`,
	} {
		got, ok := signedValue(s)
		if !ok {
			continue // the slow path handles it
		}
		if want, wok := slowValue(s); !wok || got != want {
			t.Errorf("%q: fast %#v, slow %#v (ok=%v)", s, got, want, wok)
		}
	}
}
