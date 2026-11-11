// Package soulindex keeps the souls of an in-memory store sorted, so a
// range of them can be listed without looking at the others.
package soulindex

import (
	"slices"
	"strings"
)

// Index is a sorted set of souls. New souls are buffered and merged in on
// the next query, so adding one costs nothing until souls are listed. It
// is not safe for concurrent use.
type Index struct {
	sorted []string
	added  []string
}

// Add records a soul the index does not hold yet.
func (x *Index) Add(soul string) { x.added = append(x.added, soul) }

// Between returns the souls that start with prefix and lie between from
// and to, both inclusive, in byte order. An empty from or to is no bound.
func (x *Index) Between(prefix, from, to string) []string {
	if len(x.added) > 0 {
		slices.Sort(x.added)
		x.sorted = mergeSorted(x.sorted, x.added)
		x.added = x.added[:0]
	}
	lo := max(prefix, from)
	i, _ := slices.BinarySearch(x.sorted, lo)
	var out []string
	for _, soul := range x.sorted[i:] {
		if !strings.HasPrefix(soul, prefix) || (to != "" && soul > to) {
			break // souls with prefix are contiguous, and sorted
		}
		out = append(out, soul)
	}
	return out
}

// mergeSorted merges b into a, both sorted, in place when a has room.
func mergeSorted(a, b []string) []string {
	n := len(a)
	a = slices.Grow(a, len(b))[:n+len(b)]
	i, j, k := n-1, len(b)-1, len(a)-1
	for j >= 0 {
		if i >= 0 && a[i] > b[j] {
			a[k] = a[i]
			i--
		} else {
			a[k] = b[j]
			j--
		}
		k--
	}
	return a
}
