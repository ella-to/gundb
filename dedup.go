package gundb

import (
	"hash/maphash"
	"sync"
	"sync/atomic"
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
//
// The table is split in shards by that hash, each with its own lock, so the
// read loops of many peers don't all wait for one lock. Rotation is still
// one step for the whole table.
type dup struct {
	ttl  time.Duration
	now  func() time.Time
	seed maphash.Seed

	baseOnce sync.Once
	base     time.Time    // entry times count from here
	rotated  atomic.Int64 // entry time of the last rotation

	shards [dupShards]dupShard
}

// dupShards is how many locks the table is split over.
const dupShards = 64

type dupShard struct {
	mu        sync.Mutex
	cur, prev map[uint64]dupEntry
	_         [40]byte // one cache line each, so cores don't fight over them
}

type dupEntry struct {
	at  int64 // nanoseconds since base
	via *peer // nil for messages we created
}

func newDup(ttl time.Duration) *dup {
	if ttl <= 0 {
		ttl = 9 * time.Second // gun default: opt.age = 9s
	}
	d := &dup{ttl: ttl, now: time.Now, seed: maphash.MakeSeed()}
	for i := range d.shards {
		d.shards[i].cur, d.shards[i].prev = map[uint64]dupEntry{}, map[uint64]dupEntry{}
	}
	return d
}

// clock returns the current time as an entry time.
func (d *dup) clock() int64 {
	now := d.now()
	d.baseOnce.Do(func() { d.base = now })
	return int64(now.Sub(d.base))
}

// rotate drops the older generation of every shard once ttl has passed
// since the last rotation. Every entry in it was added before the last
// rotation, at least ttl ago, so all of it has expired.
func (d *dup) rotate(at int64) {
	last := d.rotated.Load()
	if at-last < int64(d.ttl) || !d.rotated.CompareAndSwap(last, at) {
		return
	}
	for i := range d.shards {
		s := &d.shards[i]
		s.mu.Lock()
		s.prev, s.cur = s.cur, map[uint64]dupEntry{}
		s.mu.Unlock()
	}
}

func (d *dup) shard(k uint64) *dupShard { return &d.shards[k%dupShards] }

// track records id and reports whether it was already known. Tracking a
// known id refreshes its age but keeps the original sender.
func (d *dup) track(id string, via *peer) (seen bool) {
	at := d.clock()
	d.rotate(at)
	k := maphash.String(d.seed, id)
	s := d.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.lookup(k, at, d.ttl); ok {
		e.at = at
		s.cur[k] = e
		delete(s.prev, k)
		return true
	}
	s.cur[k] = dupEntry{at: at, via: via}
	return false
}

// via returns the peer that sent message id, if it is still remembered.
func (d *dup) via(id string) *peer {
	at := d.clock()
	k := maphash.String(d.seed, id)
	s := d.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, _ := s.lookup(k, at, d.ttl)
	return e.via
}

// lookup finds an unexpired entry. Callers hold s.mu.
func (s *dupShard) lookup(k uint64, at int64, ttl time.Duration) (dupEntry, bool) {
	e, ok := s.cur[k]
	if !ok {
		e, ok = s.prev[k]
	}
	if !ok || at-e.at > int64(ttl) {
		return dupEntry{}, false
	}
	return e, true
}

// size returns the number of unexpired IDs.
func (d *dup) size() int {
	at := d.clock()
	n := 0
	for i := range d.shards {
		s := &d.shards[i]
		s.mu.Lock()
		for _, e := range s.cur {
			if at-e.at <= int64(d.ttl) {
				n++
			}
		}
		for k, e := range s.prev {
			if _, dup := s.cur[k]; !dup && at-e.at <= int64(d.ttl) {
				n++
			}
		}
		s.mu.Unlock()
	}
	return n
}

// held returns how many IDs the table holds, expired ones included.
func (d *dup) held() int {
	n := 0
	for i := range d.shards {
		s := &d.shards[i]
		s.mu.Lock()
		n += len(s.cur) + len(s.prev)
		s.mu.Unlock()
	}
	return n
}
