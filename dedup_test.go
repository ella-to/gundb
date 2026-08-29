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
