package gundb

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"
)

// slowTree is toTree without plainTree: what it did before.
func slowTree(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	return out, dec.Decode(&out)
}

// sameLeaves reports whether a and b, two trees, hold the same values.
func sameLeaves(t *testing.T, a, b any) bool {
	t.Helper()
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok != bok {
		return false
	}
	if aok {
		if len(am) != len(bm) {
			return false
		}
		for k := range am {
			if !sameLeaves(t, am[k], bm[k]) {
				return false
			}
		}
		return true
	}
	av, aerr := leaf(a)
	bv, berr := leaf(b)
	return (aerr == nil) == (berr == nil) && av == bv
}

func TestPlainTreeMatchesEncoding(t *testing.T) {
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	for _, v := range []any{
		"text", "", true, nil, 0, -1, 42, int64(1) << 60, uint64(math.MaxUint64), 3.25, float32(0.1), -0.0, 1e300,
		map[string]any{"a": 1, "b": map[string]any{"c": "d", "e": nil}, "f": false},
		map[string]any(nil),
		"bad \xff utf-8", map[string]any{"bad \xff": 1}, map[string]any{"k": "bad \xff"},
		math.NaN(), math.Inf(1), json.Number("12.5"), []any{1}, struct{ A int }{1}, cyclic,
	} {
		fast, ok := plainTree(v, 0)
		slow, err := slowTree(v)
		if !ok {
			continue // toTree encodes these, as before
		}
		if err != nil {
			t.Fatalf("%#v: plain, but encoding fails: %v", v, err)
		}
		if !sameLeaves(t, fast, slow) {
			t.Fatalf("%#v: plain %#v, encoded %#v", v, fast, slow)
		}
	}
	for _, v := range []any{"bad \xff utf-8", math.NaN(), json.Number("1"), float32(0.1), []any{}, struct{}{}, cyclic, map[string]any{"bad \xff": 1}} {
		if _, ok := plainTree(v, 0); ok {
			t.Fatalf("%#v must be encoded", v)
		}
	}
}
