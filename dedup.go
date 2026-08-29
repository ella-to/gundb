package gundb

import (
	"sync"
	"time"
)

// dup is the DAM message table from gun/src/dup.js. Every message ID seen or
// sent is remembered for ttl, so loops in the mesh are dropped. It also
// remembers which peer a message came from, which is how acks find their
// way back to whoever asked.
type dup struct {
	ttl time.Duration
	now func() time.Time

	mu        sync.Mutex
	seen      map[string]dupEntry
	lastSweep time.Time
}

type dupEntry struct {
	at  time.Time
	via *peer // nil for messages we created
}

func newDup(ttl time.Duration) *dup {
	if ttl <= 0 {
		ttl = 9 * time.Second // gun default: opt.age = 9s
	}
	return &dup{ttl: ttl, now: time.Now, seen: map[string]dupEntry{}}
}

// track records id and reports whether it was already known. Tracking a
// known id refreshes its age but keeps the original sender.
func (d *dup) track(id string, via *peer) (seen bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if now.Sub(d.lastSweep) > d.ttl {
		for k, e := range d.seen {
			if now.Sub(e.at) > d.ttl {
				delete(d.seen, k)
			}
		}
		d.lastSweep = now
	}
	e, ok := d.seen[id]
	if ok && now.Sub(e.at) <= d.ttl {
		e.at = now
		d.seen[id] = e
		return true
	}
	d.seen[id] = dupEntry{at: now, via: via}
	return false
}

// via returns the peer that sent message id, if it is still remembered.
func (d *dup) via(id string) *peer {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e, ok := d.seen[id]; ok && d.now().Sub(e.at) <= d.ttl {
		return e.via
	}
	return nil
}

func (d *dup) size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}
