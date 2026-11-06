package gundb

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"ella.to/gundb/sea"
)

// ErrQuota is returned by PutAck when a write would take its owner past
// their Limit (see Options.Quota).
var ErrQuota = errors.New("gundb: quota exceeded")

// Quota limits how much data each owner may keep on a DB, for plans like
// "free: 1,000 nodes and 10 MB, pro: 10,000 nodes and 100 MB".
//
// Every node belongs to at most one owner, decided by its soul. The DB
// keeps a running total per owner, so checking a write costs a map lookup
// and a call to Limit, not a scan:
//
//   - A node counts while it has at least one field that is not null.
//     Deleting every field (put null) frees the node. In a user's space a
//     deletion is a signed null, and counts as one.
//   - Its bytes are the sum, over its non-null fields, of the field name's
//     length plus the value's: a string's bytes (signed and encrypted
//     values included, as stored), a link's soul, 8 for a number, 1 for a
//     bool. Overwriting a value counts the difference, deleting it frees it.
//
// Writes from peers that connect to the DB are checked after CanWrite, and
// rejected whole, with ErrQuota, if they would take any owner past a
// limit. A write that does not grow an owner's usage is always allowed, so
// an owner over the limit (after a downgrade, say) can still delete. The
// DB's own writes and data from peers it dialled are counted but never
// refused, like CanWrite.
//
// The totals live in memory. New rebuilds them from the store by reading
// every node once, which needs a SoulStore (the default store and
// pebblestore are); with another store they start from zero.
type Quota struct {
	// Owner returns whose quota the node soul counts against, or "" for
	// nobody (the node is not counted or limited). Default: the SEA user
	// whose space the soul is in (~<pub> and ~<pub>/...), so each user
	// pays for their own data, and nobody can write to it but them.
	Owner func(soul string) string

	// Limit returns owner's limits: look up their plan. It is called for
	// every checked write that grows owner's usage, so keep it fast and in
	// memory. Default: no limits, only counting.
	Limit func(owner string) Limit
}

// Limit is the most an owner may keep. Zero means no limit.
type Limit struct {
	Nodes int64
	Bytes int64
}

// Usage is how much an owner keeps: nodes with at least one non-null
// field, and their bytes (see Quota).
type Usage struct {
	Nodes int64
	Bytes int64
}

// LiveReader is an optional extension of Store for Options.Quota. When a
// write to a FieldReader store may create or empty a node, the DB has to
// know whether the node has other non-null fields than the ones written;
// HasLive answers that without reading the whole node. Without it, the DB
// reads the node with Get.
//
// HasLive reports whether soul has a field, other than those in skip, that
// is not deleted (see IsDeleted). It should stop at the first one it finds.
type LiveReader interface {
	Store
	HasLive(ctx context.Context, soul string, skip []string) (bool, error)
}

// Usage returns how much owner keeps on this DB. It is zero without
// Options.Quota.
func (db *DB) Usage(owner string) Usage {
	if db.quota == nil {
		return Usage{}
	}
	db.quota.mu.Lock()
	defer db.quota.mu.Unlock()
	return db.quota.usage[owner]
}

// Usages returns the usage of every owner that keeps something, for a
// billing job or an admin page. It is nil without Options.Quota.
func (db *DB) Usages() map[string]Usage {
	if db.quota == nil {
		return nil
	}
	db.quota.mu.Lock()
	defer db.quota.mu.Unlock()
	return maps.Clone(db.quota.usage)
}

// quota is Options.Quota at work: the running totals.
type quota struct {
	owner func(string) string
	limit func(string) Limit

	mu    sync.Mutex
	usage map[string]Usage // only owners with something stored
}

func newQuota(q *Quota) *quota {
	t := &quota{owner: q.Owner, limit: q.Limit, usage: map[string]Usage{}}
	if t.owner == nil {
		t.owner = seaOwner
	}
	return t
}

// seaOwner is the default Quota.Owner: the user whose space soul is in.
// Alias nodes (~@alias) list who took a name; they are nobody's.
func seaOwner(soul string) string {
	if strings.HasPrefix(soul, "~@") {
		return ""
	}
	return sea.PubOf(soul)
}

// ownerDelta is how one write changes one owner's usage.
type ownerDelta struct {
	owner string
	d     Usage
}

// addDelta adds d to owner's entry in ds.
func addDelta(ds []ownerDelta, owner string, d Usage) []ownerDelta {
	if d == (Usage{}) {
		return ds
	}
	for i := range ds {
		if ds[i].owner == owner {
			ds[i].d.Nodes += d.Nodes
			ds[i].d.Bytes += d.Bytes
			return ds
		}
	}
	return append(ds, ownerDelta{owner, d})
}

// reserve adds ds to the totals. With enforce, it first checks that no
// owner whose usage grows ends up past their limit, and changes nothing
// if one would.
func (q *quota) reserve(ds []ownerDelta, enforce bool) error {
	var limits []Limit
	if enforce && q.limit != nil {
		limits = make([]Limit, len(ds))
		for i, d := range ds {
			if d.d.Nodes > 0 || d.d.Bytes > 0 {
				limits[i] = q.limit(d.owner) // outside the lock: it is the caller's code
			}
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, d := range limits {
		u := q.usage[ds[i].owner]
		if over(u.Nodes, ds[i].d.Nodes, d.Nodes) {
			return fmt.Errorf("%w: %s may keep %d nodes, this write would make it %d", ErrQuota, ds[i].owner, d.Nodes, u.Nodes+ds[i].d.Nodes)
		}
		if over(u.Bytes, ds[i].d.Bytes, d.Bytes) {
			return fmt.Errorf("%w: %s may keep %d bytes, this write would make it %d", ErrQuota, ds[i].owner, d.Bytes, u.Bytes+ds[i].d.Bytes)
		}
	}
	for _, d := range ds {
		q.addLocked(d.owner, d.d)
	}
	return nil
}

// over reports whether adding d to have breaks limit; shrinking never does.
func over(have, d, limit int64) bool { return limit > 0 && d > 0 && have+d > limit }

// release takes back what reserve added, for a write the store failed.
func (q *quota) release(ds []ownerDelta) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, d := range ds {
		q.addLocked(d.owner, Usage{-d.d.Nodes, -d.d.Bytes})
	}
}

func (q *quota) addLocked(owner string, d Usage) {
	u := q.usage[owner]
	u.Nodes += d.Nodes
	u.Bytes += d.Bytes
	if u == (Usage{}) {
		delete(q.usage, owner)
		return
	}
	q.usage[owner] = u
}

// load counts what store already holds: every node, read once.
func (q *quota) load(ctx context.Context, store Store) error {
	ss, ok := store.(SoulStore)
	if !ok {
		return errors.New("gundb: the store cannot list souls, so quota usage starts from zero")
	}
	souls, err := ss.ListSouls(ctx, "")
	if err != nil {
		return err
	}
	for _, soul := range souls {
		owner := q.owner(soul)
		if owner == "" {
			continue
		}
		n, err := store.Get(ctx, soul)
		if err != nil {
			return err
		}
		if n == nil {
			continue
		}
		var u Usage
		for k, v := range n.Fields {
			u.Bytes += fieldSize(k, v)
			if isLive(v) {
				u.Nodes = 1
			}
		}
		q.mu.Lock()
		q.addLocked(owner, u)
		q.mu.Unlock()
	}
	return nil
}

// signedNull starts a deletion in a user's space, where every value is
// signed, nulls too: {":":null,"~":"<signature>"} (see sea.SignField).
const signedNull = `{":":null,"~":`

// IsDeleted reports whether v deletes its field: null, or a signed null
// in a user's space. Stores use it to implement LiveReader.
func IsDeleted(v Value) bool {
	switch x := v.(type) {
	case Null:
		return true
	case String:
		return strings.HasPrefix(string(x), signedNull)
	}
	return false
}

// isLive reports whether v is a value, not a deletion or nothing.
func isLive(v Value) bool { return v != nil && !IsDeleted(v) }

// fieldSize is what field k holding v counts for (see Quota).
func fieldSize(k string, v Value) int64 {
	if !isLive(v) {
		return 0
	}
	switch x := v.(type) {
	case String:
		return int64(len(k) + len(x))
	case Link:
		return int64(len(k) + len(x.Soul))
	case Number:
		return int64(len(k) + 8)
	case Bool:
		return int64(len(k) + 1)
	}
	return 0
}

// usageDelta works out how merging in into soul changes its owner's usage.
// cur is the node after the merge, olds the values the accepted fields
// held before it (nil if absent), and full whether cur holds every field
// of the node or only those of in (FieldReader). It runs before the store
// is written, so the store still holds the node as it was.
func (db *DB) usageDelta(ctx context.Context, soul string, in, cur *Node, olds map[string]Value, full bool) (Usage, error) {
	var d Usage
	for k, old := range olds {
		d.Bytes += fieldSize(k, cur.Fields[k]) - fieldSize(k, old)
	}
	// Whether the node is live (has a non-null field) can only change if
	// the fields written went from none live to some, or back.
	var before, after bool
	for k := range in.Fields {
		now := cur.Fields[k]
		was, changed := olds[k]
		if !changed {
			was = now
		}
		before = before || isLive(was)
		after = after || isLive(now)
	}
	if before == after {
		return d, nil
	}
	rest, err := db.liveElsewhere(ctx, soul, in, cur, full)
	if err != nil || rest {
		return d, err
	}
	if after {
		d.Nodes = 1
	} else {
		d.Nodes = -1
	}
	return d, nil
}

// liveElsewhere reports whether soul has a non-null field that in does
// not write.
func (db *DB) liveElsewhere(ctx context.Context, soul string, in, cur *Node, full bool) (bool, error) {
	if full {
		for k, v := range cur.Fields {
			if _, written := in.Fields[k]; !written && isLive(v) {
				return true, nil
			}
		}
		return false, nil
	}
	skip := make([]string, 0, len(in.Fields))
	for k := range in.Fields {
		skip = append(skip, k)
	}
	if lr, ok := db.store.(LiveReader); ok {
		return lr.HasLive(ctx, soul, skip)
	}
	n, err := db.store.Get(ctx, soul)
	if err != nil || n == nil {
		return false, err
	}
	return HasLiveField(n, skip), nil
}

// HasLiveField reports whether n has a field, other than those in skip,
// that is not null: LiveReader.HasLive for a node in hand. Stores use it
// to implement HasLive.
func HasLiveField(n *Node, skip []string) bool {
	for k, v := range n.Fields {
		if isLive(v) && !skipped(skip, k) {
			return true
		}
	}
	return false
}

// skipped reports whether k is in skip, which is short: the fields of one
// write.
func skipped(skip []string, k string) bool { return slices.Contains(skip, k) }
