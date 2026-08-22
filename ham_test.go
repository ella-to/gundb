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
