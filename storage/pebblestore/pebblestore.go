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
// without bound; nothing is pruned.
package pebblestore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"math"
	"sync"

	"ella.to/gundb"
	"github.com/cockroachdb/pebble/v2"
)

// Options tunes a Store. The zero value is a good default.
type Options struct {
	// NoSync skips the fsync after each write. Writes are much faster, but
	// the last ones can be lost in a crash (not corrupted).
	NoSync bool
	// Pebble overrides the engine options.
	Pebble *pebble.Options
}

// Store is a gundb.FieldStore backed by Pebble. It is safe for concurrent use.
type Store struct {
	db    *pebble.DB
	sync  *pebble.WriteOptions
	close func() error
}

var _ gundb.FieldStore = (*Store)(nil)

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
		po = &pebble.Options{Logger: quiet{}}
	}
	db, err := pebble.Open(path, po)
	if err != nil {
		return nil, fmt.Errorf("pebblestore: %w", err)
	}
	s := &Store{db: db, sync: pebble.Sync, close: sync.OnceValue(db.Close)}
	if o.NoSync {
		s.sync = pebble.NoSync
	}
	return s, nil
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

// Put implements gundb.Store: every field of n is written.
func (s *Store) Put(_ context.Context, n *gundb.Node) error { return s.write(n.Soul, n) }

// PutFields implements gundb.FieldStore: only the changed fields are written.
func (s *Store) PutFields(_ context.Context, soul string, changed *gundb.Node) error {
	return s.write(soul, changed)
}

func (s *Store) write(soul string, n *gundb.Node) error {
	b := s.db.NewBatch()
	defer b.Close()
	lp, hp := latestPrefix(soul), historyPrefix(soul)
	var buf []byte
	for field, v := range n.Fields {
		state := n.States[field]
		enc, err := encodeValue(v)
		if err != nil {
			return fmt.Errorf("pebblestore: %q.%q: %w", soul, field, err)
		}
		buf = binary.BigEndian.AppendUint64(buf[:0], math.Float64bits(state))
		buf = append(buf, enc...)
		b.Set(append(lp[:len(lp):len(lp)], field...), buf, nil)
		b.Set(historyKey(hp, field, state), enc, nil)
	}
	if err := b.Commit(s.sync); err != nil {
		return fmt.Errorf("pebblestore: %w", err)
	}
	return nil
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

// ---- keys ----
//
// Souls and fields are escaped so they can be followed by more key bytes and
// still sort correctly: 0x00 becomes 0x00 0xff, and 0x00 0x01 ends them.

func escape(s string) []byte {
	b := make([]byte, 0, len(s)+2)
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

func encodeValue(v gundb.Value) ([]byte, error) {
	switch x := v.(type) {
	case gundb.Null:
		return []byte{tNull}, nil
	case gundb.Bool:
		if x {
			return []byte{tTrue}, nil
		}
		return []byte{tFalse}, nil
	case gundb.Number:
		return binary.BigEndian.AppendUint64([]byte{tNumber}, math.Float64bits(float64(x))), nil
	case gundb.String:
		return append([]byte{tString}, x...), nil
	case gundb.Link:
		return append([]byte{tLink}, x.Soul...), nil
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
