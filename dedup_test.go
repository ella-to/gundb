package gundb

import (
	"testing"
	"time"
)

func TestDup(t *testing.T) {
	now := time.Unix(0, 0)
	d := newDup(time.Second)
	d.now = func() time.Time { return now }
	p := &peer{}

	if d.track("a", p) {
		t.Fatal("first sighting reported as seen")
	}
	if !d.track("a", nil) {
		t.Fatal("second sighting not reported")
	}
	if d.via("a") != p {
		t.Fatal("tracking again must keep the original sender")
	}
	now = now.Add(900 * time.Millisecond)
	d.track("a", nil) // refresh
	now = now.Add(900 * time.Millisecond)
	if d.via("a") != p {
		t.Fatal("refresh did not extend the entry")
	}
	now = now.Add(2 * time.Second)
	d.track("b", nil) // triggers a sweep
	if d.via("a") != nil || d.size() != 1 {
		t.Fatalf("expired entry kept, size=%d", d.size())
	}
}

// IDs live in two generations; check that rotating keeps live IDs, frees
// expired ones, and never stalls on a sweep.
func TestDupGenerations(t *testing.T) {
	now := time.Unix(0, 0)
	d := newDup(time.Second)
	d.now = func() time.Time { return now }
	p := &peer{}

	d.track("a", p)
	now = now.Add(1500 * time.Millisecond)
	d.track("b", nil) // rotates: "a" moves to the old generation
	if d.via("a") != nil {
		t.Fatal("a is 1.5s old and must be expired")
	}
	if d.track("a", nil) {
		t.Fatal("an expired id must count as new")
	}
	if d.size() != 2 {
		t.Fatalf("size = %d, want 2 (a, b)", d.size())
	}

	now = now.Add(900 * time.Millisecond)
	if !d.track("b", nil) || d.via("b") != nil {
		t.Fatal("b is 0.9s old: still seen, and created by us")
	}
	now = now.Add(200 * time.Millisecond)
	d.track("c", nil) // rotates again
	if !d.track("b", nil) {
		t.Fatal("b was refreshed 0.2s ago and must survive the rotation")
	}

	now = now.Add(5 * time.Second)
	d.track("x", nil)
	d.track("y", nil)
	now = now.Add(time.Second)
	d.track("z", nil) // two rotations later only recent IDs are held
	if held := len(d.cur) + len(d.prev); held != 3 {
		t.Fatalf("holding %d IDs, want 3 (x, y, z)", held)
	}
}
