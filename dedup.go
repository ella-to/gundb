package gundb

import (
	"sync"
	"time"
)

// dup is the DAM message table from gun/src/dup.js. Every message ID seen or
// sent is remembered for ttl, so loops in the mesh are dropped. It also
// remembers which peer a message came from, which is how acks find their
// way back to whoever asked.
//
// IDs are kept in two generations instead of one map that is swept: every
// ttl the older generation is dropped whole and the current one takes its
// place. Rotating is O(1), so a busy relay holding millions of IDs never
// stalls every message behind a sweep. Everything in a dropped generation
// is older than ttl, and lookups still check each entry's age, so IDs
// expire exactly as before; they just free their memory up to ttl later.
type dup struct {
	ttl time.Duration
	now func() time.Time

	mu        sync.Mutex
	cur, prev map[string]dupEntry
	rotated   time.Time
}

type dupEntry struct {
	at  time.Time
	via *peer // nil for messages we created
}

func newDup(ttl time.Duration) *dup {
	if ttl <= 0 {
		ttl = 9 * time.Second // gun default: opt.age = 9s
	}
	return &dup{ttl: ttl, now: time.Now, cur: map[string]dupEntry{}, prev: map[string]dupEntry{}}
}

// track records id and reports whether it was already known. Tracking a
// known id refreshes its age but keeps the original sender.
func (d *dup) track(id string, via *peer) (seen bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if now.Sub(d.rotated) >= d.ttl {
		// Every entry in prev was added before the last rotation, at least
		// ttl ago, so all of it has expired.
		d.prev, d.cur, d.rotated = d.cur, map[string]dupEntry{}, now
	}
	if e, ok := d.lookup(id, now); ok {
		e.at = now
		d.cur[id] = e
		delete(d.prev, id)
		return true
	}
	d.cur[id] = dupEntry{at: now, via: via}
	return false
}

// via returns the peer that sent message id, if it is still remembered.
func (d *dup) via(id string) *peer {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, _ := d.lookup(id, d.now())
	return e.via
}

// lookup finds an unexpired entry. Callers hold mu.
func (d *dup) lookup(id string, now time.Time) (dupEntry, bool) {
	e, ok := d.cur[id]
	if !ok {
		e, ok = d.prev[id]
	}
	if !ok || now.Sub(e.at) > d.ttl {
		return dupEntry{}, false
	}
	return e, true
}

// size returns the number of unexpired IDs.
func (d *dup) size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	now, n := d.now(), 0
	for _, e := range d.cur {
		if now.Sub(e.at) <= d.ttl {
			n++
		}
	}
	for id, e := range d.prev {
		if _, dup := d.cur[id]; !dup && now.Sub(e.at) <= d.ttl {
			n++
		}
	}
	return n
}
