package gundb

import (
	"math"
	"testing"
)

func TestHAM(t *testing.T) {
	inf := math.Inf(-1)
	cases := []struct {
		name             string
		machine, in, cur float64
		inVal, curVal    Value
		want             hamResult
	}{
		{"future write is deferred", 10, 11, 5, String("a"), String("b"), hamDefer},
		{"older write is historical", 10, 4, 5, String("a"), String("b"), hamHistorical},
		{"newer write wins", 10, 6, 5, String("a"), String("b"), hamIncoming},
		{"first write wins", 10, 6, inf, String("a"), nil, hamIncoming},
		{"same value same state", 10, 5, 5, String("a"), String("a"), hamSame},
		{"tie: larger JSON wins", 10, 5, 5, String("b"), String("a"), hamIncoming},
		{"tie: smaller JSON loses", 10, 5, 5, String("a"), String("b"), hamCurrent},
		{"tie across types uses JSON text", 10, 5, 5, Number(1), String("1"), hamIncoming}, // `1` > `"1"`
	}
	for _, c := range cases {
		if got := ham(c.machine, c.in, c.cur, c.inVal, c.curVal); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestApplyConverges(t *testing.T) {
	// Two peers receiving the same conflicting writes in opposite order end
	// up with the same value.
	w1 := graph{"x": {Soul: "x", Fields: map[string]Value{"v": String("one")}, States: map[string]float64{"v": 100}}}
	w2 := graph{"x": {Soul: "x", Fields: map[string]Value{"v": String("two")}, States: map[string]float64{"v": 100}}}
	a, b := New(), New()
	defer a.Close()
	defer b.Close()
	a.apply(t.Context(), w1)
	a.apply(t.Context(), w2)
	b.apply(t.Context(), w2)
	b.apply(t.Context(), w1)
	va, _ := a.Get("x").Get("v").Once[string](t.Context())
	vb, _ := b.Get("x").Get("v").Once[string](t.Context())
	if va != "two" || vb != "two" {
		t.Fatalf("diverged: %q vs %q", va, vb)
	}
}

func TestFutureWriteIsAppliedLater(t *testing.T) {
	db := New()
	defer db.Close()
	soon := float64(db.state.nowMs() + 150)
	db.apply(t.Context(), graph{"x": {Soul: "x", Fields: map[string]Value{"v": Bool(true)}, States: map[string]float64{"v": soon}}})
	if _, err := db.Get("x").Get("v").Once[bool](t.Context()); err != ErrNotFound {
		t.Fatal("future write applied too early")
	}
	eventually(t, func() bool {
		v, _ := db.Get("x").Get("v").Once[bool](t.Context())
		return v
	})
}
