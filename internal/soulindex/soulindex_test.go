package soulindex

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

func TestBetween(t *testing.T) {
	var x Index
	var all []string
	r := rand.New(rand.NewPCG(1, 2))
	for round := range 5 {
		for range 200 {
			soul := fmt.Sprintf("%c/%03d", "abc"[r.IntN(3)], r.IntN(1000))
			if !slices.Contains(all, soul) {
				all = append(all, soul)
				x.Add(soul)
			}
		}
		for _, q := range [][3]string{
			{"", "", ""}, {"b/", "", ""}, {"b/", "b/100", "b/500"}, {"", "a/900", "c/100"},
			{"c/", "b/000", ""}, {"a/", "", "a/050"}, {"z", "", ""}, {"b/1", "b/150", "b/199"},
		} {
			var want []string
			for _, s := range all {
				if strings.HasPrefix(s, q[0]) && (q[1] == "" || s >= q[1]) && (q[2] == "" || s <= q[2]) {
					want = append(want, s)
				}
			}
			slices.Sort(want)
			if got := x.Between(q[0], q[1], q[2]); !slices.Equal(got, want) {
				t.Fatalf("round %d, %q: got %v, want %v", round, q, got, want)
			}
		}
	}
}
