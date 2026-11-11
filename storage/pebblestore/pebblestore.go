// Package pebblestore is a gundb.Store on Pebble, an LSM-tree key-value
// engine (the one behind CockroachDB). Writes are appends to a write-ahead
// log, reads are short scans over sorted keys, and every accepted write is
// kept, so past versions of a field can be read back.
//
//	store, err := pebblestore.Open("data")
//	db := gundb.New(gundb.Options{Store: store})
//	defer store.Close()
//
// Two kinds of keys are written, in one atomic batch per merge:
//
//	L <soul> <field>         -> state, value   the latest version
//	H <soul> <field> <state> -> value          every version, newest first
//
// History holds the writes that won HAM, deletions (null) included. It grows
// without bound unless pruned with PruneHistory, or turned off with
// Options.NoHistory.
package pebblestore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"math"
	"slices"
	"strings"
	"sync"

	"ella.to/gundb"
	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/bloom"
)

// Options tunes a Store. The zero value is a good default.
type Options struct {
	// NoSync skips the fsync after each write. Writes are much faster, but
	// the last ones can be lost in a crash (not corrupted).
	NoSync bool
	// NoHistory keeps only the latest version of each field: History and
	// GetAt find nothing, and every write stores half the keys. For relays
	// that never read history.
	NoHistory bool
	// CacheSize is the most memory Pebble's block cache may use. Default:
	// 64 MB. It is only taken as blocks are read.
	CacheSize int64
	// Pebble overrides the engine options; CacheSize and the Bloom filters
	// Open sets by default are then left to it.
	Pebble *pebble.Options
}

// DefaultCacheSize is Options.CacheSize when it is not set.
const DefaultCacheSize = 64 << 20

// Store is a gundb.FieldReader, gundb.BatchStore, gundb.SoulStore and
// gundb.LiveReader backed by Pebble. It is
// safe for concurrent use.
type Store struct {
	db        *pebble.DB
	sync      *pebble.WriteOptions
	noHistory bool
	close     func() error
}

var (
	_ gundb.FieldReader = (*Store)(nil)
	_ gundb.BatchStore  = (*Store)(nil)
	_ gundb.SoulStore   = (*Store)(nil)
	_ gundb.LiveReader  = (*Store)(nil)
)

// Version is one past write of a field.
type Version struct {
	State float64
	Value gundb.Value
}

// Open opens or creates a store in the directory path.
func Open(path string, opts ...Options) (*Store, error) {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	po := o.Pebble
	if po == nil {
		po = defaultOptions(o.CacheSize)
	}
	db, err := pebble.Open(path, po)
	if err != nil {
		return nil, fmt.Errorf("pebblestore: %w", err)
	}
	s := &Store{db: db, sync: pebble.Sync, noHistory: o.NoHistory, close: sync.OnceValue(db.Close)}
	if o.NoSync {
		s.sync = pebble.NoSync
	}
	return s, nil
}

// defaultOptions are the engine options Open uses. Every merge looks up
// the fields it writes, most of which are new, and a Bloom filter answers
// most lookups of a key that is not there without reading a table.
func defaultOptions(cacheSize int64) *pebble.Options {
	if cacheSize <= 0 {
		cacheSize = DefaultCacheSize
	}
	po := &pebble.Options{Logger: quiet{}, CacheSize: cacheSize}
	for i := range po.Levels {
		po.Levels[i].FilterPolicy = bloom.FilterPolicy(10)
	}
	return po
}

// Close flushes and closes the store. Calling it again does nothing.
func (s *Store) Close() error { return s.close() }

// Get implements gundb.Store.
func (s *Store) Get(_ context.Context, soul string) (*gundb.Node, error) {
	prefix := latestPrefix(soul)
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: upper(prefix)})
	if err != nil {
		return nil, fmt.Errorf("pebblestore: %w", err)
	}
	defer it.Close()
	var n *gundb.Node
	for it.First(); it.Valid(); it.Next() {
		v := it.Value()
		if len(v) < 8 {
			return nil, fmt.Errorf("pebblestore: corrupt value for %q", soul)
		}
		val, err := decodeValue(v[8:])
		if err != nil {
			return nil, err
		}
		if n == nil {
			n = gundb.NewNode(soul)
		}
		n.Set(string(it.Key()[len(prefix):]), val, math.Float64frombits(binary.BigEndian.Uint64(v)))
	}
	return n, it.Error()
}

// GetFields implements gundb.FieldReader with one point lookup per field,
// so a merge into a node with many fields reads only what it writes.
func (s *Store) GetFields(_ context.Context, soul string, fields []string) (*gundb.Node, error) {
	prefix := latestPrefix(soul)
	var n *gundb.Node
	var key []byte
	for _, field := range fields {
		key = append(append(key[:0], prefix...), field...) // L keys end with the raw field
		v, closer, err := s.db.Get(key)
		if errors.Is(err, pebble.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("pebblestore: %w", err)
		}
		if len(v) < 8 {
			closer.Close()
			return nil, fmt.Errorf("pebblestore: corrupt value for %q", soul)
		}
		val, err := decodeValue(v[8:])
		state := math.Float64frombits(binary.BigEndian.Uint64(v))
		closer.Close()
		if err != nil {
			return nil, err
		}
		if n == nil {
			n = gundb.NewNode(soul)
		}
		n.Set(field, val, state)
	}
	return n, nil
}

// HasLive implements gundb.LiveReader: it walks the node's latest values
// and stops at the first one that is not deleted, without decoding any.
func (s *Store) HasLive(_ context.Context, soul string, skip []string) (bool, error) {
	prefix := latestPrefix(soul)
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: upper(prefix)})
	if err != nil {
		return false, fmt.Errorf("pebblestore: %w", err)
	}
	defer it.Close()
	for it.First(); it.Valid(); it.Next() {
		v := it.Value()
		if len(v) < 9 {
			return false, fmt.Errorf("pebblestore: corrupt value for %q", soul)
		}
		if !deleted(v[8:]) && !slices.Contains(skip, string(it.Key()[len(prefix):])) {
			return true, nil
		}
	}
	return false, it.Error()
}

// Put implements gundb.Store: every field of n is written.
func (s *Store) Put(_ context.Context, n *gundb.Node) error { return s.write(n.Soul, n) }

// PutFields implements gundb.FieldStore: only the changed fields are written.
func (s *Store) PutFields(_ context.Context, soul string, changed *gundb.Node) error {
	return s.write(soul, changed)
}

// PutBatch implements gundb.BatchStore: the changed fields of several
// nodes are committed in one batch, with one sync.
func (s *Store) PutBatch(_ context.Context, changed []*gundb.Node) error {
	b := s.db.NewBatch()
	defer b.Close()
	var bufs keyBufs
	for _, n := range changed {
		if err := bufs.add(b, n.Soul, n, s.noHistory); err != nil {
			return err
		}
	}
	return commit(b, s.sync)
}

func (s *Store) write(soul string, n *gundb.Node) error {
	b := s.db.NewBatch()
	defer b.Close()
	var bufs keyBufs
	if err := bufs.add(b, soul, n, s.noHistory); err != nil {
		return err
	}
	return commit(b, s.sync)
}

// keyBufs are the buffers add builds keys and values in. Batch.Set copies
// what it is given, so one set of buffers serves every field of a batch.
type keyBufs struct{ lkey, hkey, val []byte }

// grow makes room for the keys and values of n, in one allocation.
func (k *keyBufs) grow(soul string, n *gundb.Node) {
	maxField, maxVal := 0, 0
	for field, v := range n.Fields {
		maxField = max(maxField, escapedLen(field))
		maxVal = max(maxVal, valueLen(v))
	}
	l := 1 + escapedLen(soul) + maxField
	h := l + 8 // the history key also ends the field with 0x00 0x01, then the state
	v := 8 + maxVal
	if cap(k.lkey) >= l && cap(k.hkey) >= h && cap(k.val) >= v {
		return
	}
	buf := make([]byte, l+h+v)
	k.lkey, k.hkey, k.val = buf[:0:l], buf[l:l:l+h], buf[l+h:l+h:l+h+v]
}

func escapedLen(s string) int { return len(s) + strings.Count(s, "\x00") + 2 }

func valueLen(v gundb.Value) int {
	switch x := v.(type) {
	case gundb.Number:
		return 9
	case gundb.String:
		return 1 + len(x)
	case gundb.Link:
		return 1 + len(x.Soul)
	}
	return 1
}

// add puts the fields of n, the latest version and (unless noHistory)
// history, in b.
func (k *keyBufs) add(b *pebble.Batch, soul string, n *gundb.Node, noHistory bool) error {
	k.grow(soul, n)
	k.lkey = appendEscaped(append(k.lkey[:0], 'L'), soul)
	lp := len(k.lkey)
	k.hkey = append(k.hkey[:0], 'H')
	k.hkey = append(k.hkey, k.lkey[1:]...)
	hp := len(k.hkey)
	for field, v := range n.Fields {
		state := n.States[field]
		k.val = binary.BigEndian.AppendUint64(k.val[:0], math.Float64bits(state))
		var err error
		if k.val, err = appendValue(k.val, v); err != nil {
			return fmt.Errorf("pebblestore: %q.%q: %w", soul, field, err)
		}
		k.lkey = append(k.lkey[:lp], field...)
		b.Set(k.lkey, k.val, nil)
		if !noHistory {
			k.hkey = binary.BigEndian.AppendUint64(appendEscaped(k.hkey[:hp], field), descending(state))
			b.Set(k.hkey, k.val[8:], nil)
		}
	}
	return nil
}

func commit(b *pebble.Batch, sync *pebble.WriteOptions) error {
	if err := b.Commit(sync); err != nil {
		return fmt.Errorf("pebblestore: %w", err)
	}
	return nil
}

// ListSouls implements gundb.SoulStore. It seeks from soul to soul, so it
// costs one step per soul, not per field.
func (s *Store) ListSouls(_ context.Context, prefix string) ([]string, error) {
	lower := latestPrefix(prefix)
	lower = lower[:len(lower)-2] // drop the terminator: any soul starting with prefix
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper(lower)})
	if err != nil {
		return nil, fmt.Errorf("pebblestore: %w", err)
	}
	defer it.Close()
	var souls []string
	for ok := it.First(); ok; {
		soul, _, err := unescape(it.Key()[1:])
		if err != nil {
			return nil, err
		}
		souls = append(souls, soul)
		next := latestPrefix(soul) // ends 0x00 0x01; 0x00 0x02 is past all its fields
		next[len(next)-1]++
		ok = it.SeekGE(next)
	}
	return souls, it.Error()
}

// History returns every stored version of a field, newest first.
func (s *Store) History(_ context.Context, soul, field string) iter.Seq2[Version, error] {
	return func(yield func(Version, error) bool) {
		prefix := append(historyPrefix(soul), escape(field)...)
		it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: upper(prefix)})
		if err != nil {
			yield(Version{}, fmt.Errorf("pebblestore: %w", err))
			return
		}
		defer it.Close()
		for it.First(); it.Valid(); it.Next() {
			ver, err := version(it.Key()[len(prefix):], it.Value())
			if !yield(ver, err) || err != nil {
				return
			}
		}
		if err := it.Error(); err != nil {
			yield(Version{}, fmt.Errorf("pebblestore: %w", err))
		}
	}
}

// GetAt returns the node as it was at state t: for each field, the newest
// version with a state <= t. It returns (nil, nil) if nothing was written
// by then.
func (s *Store) GetAt(_ context.Context, soul string, t float64) (*gundb.Node, error) {
	prefix := historyPrefix(soul)
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: upper(prefix)})
	if err != nil {
		return nil, fmt.Errorf("pebblestore: %w", err)
	}
	defer it.Close()
	var n *gundb.Node
	at := descending(t)
	for ok := it.First(); ok; {
		field, rest, err := unescape(it.Key()[len(prefix):])
		if err != nil {
			return nil, err
		}
		fieldPrefix := it.Key()[:len(it.Key())-len(rest)]
		fieldPrefix = append([]byte(nil), fieldPrefix...)
		if it.SeekGE(binary.BigEndian.AppendUint64(fieldPrefix[:len(fieldPrefix):len(fieldPrefix)], at)) &&
			bytes.HasPrefix(it.Key(), fieldPrefix) {
			ver, err := version(it.Key()[len(fieldPrefix):], it.Value())
			if err != nil {
				return nil, err
			}
			if n == nil {
				n = gundb.NewNode(soul)
			}
			n.Set(field, ver.Value, ver.State)
		}
		// Skip the rest of this field: its terminator 0x00 0x01 becomes 0x00 0x02.
		next := fieldPrefix[:len(fieldPrefix):len(fieldPrefix)]
		next[len(next)-1]++
		ok = it.SeekGE(next)
	}
	return n, it.Error()
}

// Prune says which past versions PruneHistory deletes. The latest version
// of a field is always kept.
type Prune struct {
	// Keep is how many versions of each field to keep, the latest
	// included. Zero keeps any number.
	Keep int
	// Before deletes the versions with an older state (milliseconds since
	// the Unix epoch, like every HAM state). Zero deletes none by age.
	Before float64
}

// PruneHistory deletes the past versions of every field that p selects,
// and returns how many it deleted. It reads the whole history once; run it
// from time to time, not on every write. The latest values are untouched.
func (s *Store) PruneHistory(ctx context.Context, p Prune) (int, error) {
	if p.Keep <= 0 && p.Before <= 0 {
		return 0, nil
	}
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: []byte{'H'}, UpperBound: []byte{'H' + 1}})
	if err != nil {
		return 0, fmt.Errorf("pebblestore: %w", err)
	}
	defer it.Close()
	b := s.db.NewBatch()
	defer func() { b.Close() }()
	deleted, nth := 0, 0
	var field []byte // the soul and field of the versions being counted
	for it.First(); it.Valid(); it.Next() {
		key := it.Key()
		if len(key) < 9 {
			return deleted, errors.New("pebblestore: corrupt history key")
		}
		if f := key[:len(key)-8]; !bytes.Equal(f, field) {
			field, nth = append(field[:0], f...), 0
		}
		nth++
		state := ascending(binary.BigEndian.Uint64(key[len(key)-8:]))
		if nth == 1 || (p.Keep <= 0 || nth <= p.Keep) && (p.Before <= 0 || state >= p.Before) {
			continue // newest first: the rest of this field is older still
		}
		if err := b.Delete(key, nil); err != nil {
			return deleted, fmt.Errorf("pebblestore: %w", err)
		}
		deleted++
		if b.Count() >= 10_000 {
			if err := commit(b, s.sync); err != nil {
				return deleted, err
			}
			b.Close()
			b = s.db.NewBatch()
			if err := ctx.Err(); err != nil {
				return deleted, err
			}
		}
	}
	if err := it.Error(); err != nil {
		return deleted, fmt.Errorf("pebblestore: %w", err)
	}
	return deleted, commit(b, s.sync)
}

// ---- keys ----
//
// Souls and fields are escaped so they can be followed by more key bytes and
// still sort correctly: 0x00 becomes 0x00 0xff, and 0x00 0x01 ends them.

func escape(s string) []byte { return appendEscaped(make([]byte, 0, len(s)+2), s) }

// appendEscaped appends s, escaped and terminated, to b.
func appendEscaped(b []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		b = append(b, s[i])
		if s[i] == 0 {
			b = append(b, 0xff)
		}
	}
	return append(b, 0, 1)
}

// unescape reads an escaped string from the front of b.
func unescape(b []byte) (string, []byte, error) {
	var s []byte
	for i := 0; i+1 < len(b); i++ {
		if b[i] != 0 {
			s = append(s, b[i])
			continue
		}
		switch b[i+1] {
		case 1:
			return string(s), b[i+2:], nil
		case 0xff:
			s = append(s, 0)
			i++
		default:
			return "", nil, errors.New("pebblestore: corrupt key")
		}
	}
	return "", nil, errors.New("pebblestore: corrupt key")
}

func latestPrefix(soul string) []byte  { return append([]byte{'L'}, escape(soul)...) }
func historyPrefix(soul string) []byte { return append([]byte{'H'}, escape(soul)...) }

func historyKey(prefix []byte, field string, state float64) []byte {
	k := append(prefix[:len(prefix):len(prefix)], escape(field)...)
	return binary.BigEndian.AppendUint64(k, descending(state))
}

// descending maps a state to a uint64 that sorts newest first.
func descending(f float64) uint64 {
	b := math.Float64bits(f)
	if b>>63 == 1 {
		b = ^b // negative: flip everything
	} else {
		b |= 1 << 63 // positive: flip the sign bit
	}
	return ^b
}

func ascending(u uint64) float64 {
	b := ^u
	if b>>63 == 1 {
		b &^= 1 << 63
	} else {
		b = ^b
	}
	return math.Float64frombits(b)
}

func version(stateKey, value []byte) (Version, error) {
	if len(stateKey) != 8 {
		return Version{}, errors.New("pebblestore: corrupt history key")
	}
	v, err := decodeValue(value)
	return Version{State: ascending(binary.BigEndian.Uint64(stateKey)), Value: v}, err
}

// upper returns the smallest key greater than every key with prefix p.
func upper(p []byte) []byte {
	u := bytes.Clone(p)
	for i := len(u) - 1; i >= 0; i-- {
		if u[i]++; u[i] != 0 {
			return u[:i+1]
		}
	}
	return nil
}

// ---- values: one type byte, then the payload ----

const (
	tNull byte = iota
	tFalse
	tTrue
	tNumber
	tString
	tLink
)

// deleted is gundb.IsDeleted for an encoded value: null, or a signed null
// in a user's space.
func deleted(enc []byte) bool {
	return enc[0] == tNull || (enc[0] == tString && bytes.HasPrefix(enc[1:], signedNull))
}

var signedNull = []byte(`{":":null,"~":`)

func encodeValue(v gundb.Value) ([]byte, error) { return appendValue(nil, v) }

// appendValue appends the encoding of v to b.
func appendValue(b []byte, v gundb.Value) ([]byte, error) {
	switch x := v.(type) {
	case gundb.Null:
		return append(b, tNull), nil
	case gundb.Bool:
		if x {
			return append(b, tTrue), nil
		}
		return append(b, tFalse), nil
	case gundb.Number:
		return binary.BigEndian.AppendUint64(append(b, tNumber), math.Float64bits(float64(x))), nil
	case gundb.String:
		return append(append(b, tString), x...), nil
	case gundb.Link:
		return append(append(b, tLink), x.Soul...), nil
	}
	return nil, fmt.Errorf("cannot store %T", v)
}

func decodeValue(b []byte) (gundb.Value, error) {
	if len(b) == 0 {
		return nil, errors.New("pebblestore: empty value")
	}
	switch b[0] {
	case tNull:
		return gundb.Null{}, nil
	case tFalse:
		return gundb.Bool(false), nil
	case tTrue:
		return gundb.Bool(true), nil
	case tNumber:
		if len(b) == 9 {
			return gundb.Number(math.Float64frombits(binary.BigEndian.Uint64(b[1:]))), nil
		}
	case tString:
		return gundb.String(b[1:]), nil
	case tLink:
		return gundb.Link{Soul: string(b[1:])}, nil
	}
	return nil, fmt.Errorf("pebblestore: corrupt value %x", b)
}

// quiet drops Pebble's informational logs and keeps its errors.
type quiet struct{}

func (quiet) Infof(string, ...any)              {}
func (quiet) Errorf(format string, args ...any) { pebble.DefaultLogger.Errorf(format, args...) }
func (quiet) Fatalf(format string, args ...any) { pebble.DefaultLogger.Fatalf(format, args...) }
