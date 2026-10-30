package gundb

import (
	"slices"
	"sync"
	"testing"
)

// A burst of Sets in one millisecond still gets souls in creation order.
func TestUUIDSortsInOrder(t *testing.T) {
	ids := make([]string, 50000)
	for i := range ids {
		ids[i] = uuid()
	}
	for i, id := range ids {
		if len(id) != 20 {
			t.Fatalf("%q is %d characters", id, len(id))
		}
		if i > 0 && id <= ids[i-1] {
			t.Fatalf("%q after %q", id, ids[i-1])
		}
	}
}

func TestSoulClock(t *testing.T) {
	var c soulClock
	const now = 1_700_000_000_000
	same := []string{c.soul(now), c.soul(now), c.soul(now)}
	if !slices.IsSorted(same) || same[0][8:11] != "000" || same[2][8:11] != "002" {
		t.Fatalf("%q", same)
	}

	// A clock that goes back keeps counting from the last soul.
	if back := c.soul(now - 5000); back <= same[2] {
		t.Fatalf("%q after %q", back, same[2])
	}

	// A full millisecond borrows the next one.
	c.seq = seqMax - 2
	last := c.soul(now)
	if ms, seq := c.next(now); ms != now+1 || seq != 0 {
		t.Fatalf("after overflow: %d %d", ms-now, seq)
	}
	if last[8:11] != "zzz" {
		t.Fatalf("%q", last)
	}
}

func TestUUIDConcurrent(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 2000 {
				id := uuid()
				mu.Lock()
				if seen[id] {
					t.Errorf("duplicate %q", id)
				}
				seen[id] = true
				mu.Unlock()
			}
		})
	}
	wg.Wait()
}
