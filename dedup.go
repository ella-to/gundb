package gundb

import (
	"hash/maphash"
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
//
// Entries are kept small, for relays that hold millions: the ID is hashed
// to 64 bits (GUN's IDs are 9 random characters, about 53 bits, so a hash
// collision is far less likely than two IDs being equal), and its time is
// nanoseconds since the table's first use, from the monotonic clock.
type dup struct {
	ttl  time.Duration
	now  func() time.Time
	seed maphash.Seed

	mu        sync.Mutex
	cur, prev map[uint64]dupEntry
	rotated   time.Time
	base      time.Time // entry times count from here
}

type dupEntry struct {
	at  int64 // nanoseconds since base
	via *peer // nil for messages we created
}

func newDup(ttl time.Duration) *dup {
	if ttl <= 0 {
		ttl = 9 * time.Second // gun default: opt.age = 9s
	}
	return &dup{ttl: ttl, now: time.Now, seed: maphash.MakeSeed(), cur: map[uint64]dupEntry{}, prev: map[uint64]dupEntry{}}
}

// clock returns the current time as an entry time. Callers hold mu.
func (d *dup) clock() (time.Time, int64) {
	now := d.now()
	if d.base.IsZero() {
		d.base = now
	}
	return now, int64(now.Sub(d.base))
}

// track records id and reports whether it was already known. Tracking a
// known id refreshes its age but keeps the original sender.
func (d *dup) track(id string, via *peer) (seen bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now, at := d.clock()
	if now.Sub(d.rotated) >= d.ttl {
		// Every entry in prev was added before the last rotation, at least
		// ttl ago, so all of it has expired.
		d.prev, d.cur, d.rotated = d.cur, map[uint64]dupEntry{}, now
	}
	k := maphash.String(d.seed, id)
	if e, ok := d.lookup(k, at); ok {
		e.at = at
		d.cur[k] = e
		delete(d.prev, k)
		return true
	}
	d.cur[k] = dupEntry{at: at, via: via}
	return false
}

// via returns the peer that sent message id, if it is still remembered.
func (d *dup) via(id string) *peer {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, at := d.clock()
	e, _ := d.lookup(maphash.String(d.seed, id), at)
	return e.via
}

// lookup finds an unexpired entry. Callers hold mu.
func (d *dup) lookup(k uint64, at int64) (dupEntry, bool) {
	e, ok := d.cur[k]
	if !ok {
		e, ok = d.prev[k]
	}
	if !ok || at-e.at > int64(d.ttl) {
		return dupEntry{}, false
	}
	return e, true
}

// size returns the number of unexpired IDs.
func (d *dup) size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, at := d.clock()
	n := 0
	for _, e := range d.cur {
		if at-e.at <= int64(d.ttl) {
			n++
		}
	}
	for k, e := range d.prev {
		if _, dup := d.cur[k]; !dup && at-e.at <= int64(d.ttl) {
			n++
		}
	}
	return n
}
