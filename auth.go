package gundb

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ErrForbidden is returned by PutAck when a peer's rules (Options.CanWrite)
// reject the write.
var ErrForbidden = errors.New("gundb: forbidden")

// trusted reports whether p's requests skip the rules: peers we dialled
// ourselves (Options.Peers) are trusted, only peers that connect to us are
// checked.
func (p *peer) trusted() bool { return p.origin != nil }

// allowWrite returns ErrForbidden, with the first denied field, if p may
// not write all of g.
func (db *DB) allowWrite(p *peer, g graph) error {
	if db.canWrite == nil || p.trusted() {
		return nil
	}
	for soul, n := range g {
		for field := range n.Fields {
			if !db.canWrite(p.rules, soul, field) {
				return fmt.Errorf("%w: may not write %s.%s", ErrForbidden, soul, field)
			}
		}
	}
	return nil
}

// checksReads reports whether what p reads goes through Options.CanRead.
func (db *DB) checksReads(p *peer) bool { return db.canRead != nil && !p.trusted() }

// allowRead reports whether p may read field of soul.
func (db *DB) allowRead(p *peer, soul, field string) bool {
	return !db.checksReads(p) || db.canRead(p.rules, soul, field)
}

// readableNode drops from n, in place, the fields p may not read.
func (db *DB) readableNode(p *peer, n *Node) {
	if !db.checksReads(p) {
		return
	}
	for field := range n.Fields {
		if !db.canRead(p.rules, n.Soul, field) {
			delete(n.Fields, field)
			delete(n.States, field)
		}
	}
}

// sendTo sends m, already encoded as raw, to q without the fields q may not
// read. An ack is still sent if nothing is left of its data, so whoever
// waits for it is not left hanging.
//
// When one message goes to many peers, pass the same non-nil memo for all
// of them: each distinct view of the message (which fields are left) is
// then encoded once, however many peers share it.
func (db *DB) sendTo(q *peer, m *message, raw []byte, memo *fanout) {
	if !db.checksReads(q) || len(m.Put) == 0 {
		q.send(raw)
		return
	}
	if memo == nil {
		memo = &fanout{}
	}
	if r := memo.view(db, q, m, raw); !r.none {
		q.send(r.raw)
	}
}

// fanout caches the views of one message sent to many peers. Its zero
// value is ready to use.
type fanout struct {
	fields []soulField         // every field of the message, in a fixed order
	views  map[string]readable // key: which of fields are left, as a bitmask
	seen   []byte              // scratch for the bitmask
}

type soulField struct{ soul, field string }

// readable is a message as one peer may see it.
type readable struct {
	raw  []byte
	none bool // nothing to send
}

// view returns m as q may see it.
func (f *fanout) view(db *DB, q *peer, m *message, raw []byte) readable {
	if f.fields == nil {
		for _, soul := range slices.Sorted(maps.Keys(m.Put)) {
			for _, field := range slices.Sorted(maps.Keys(m.Put[soul].Fields)) {
				f.fields = append(f.fields, soulField{soul, field})
			}
		}
		f.views = map[string]readable{}
		f.seen = make([]byte, (len(f.fields)+7)/8)
	}
	clear(f.seen)
	kept := 0
	for i, sf := range f.fields {
		if db.canRead(q.rules, sf.soul, sf.field) {
			f.seen[i/8] |= 1 << (i % 8)
			kept++
		}
	}
	if r, ok := f.views[string(f.seen)]; ok {
		return r
	}
	r := f.build(db, m, raw, kept)
	f.views[string(f.seen)] = r
	return r
}

// build encodes the view of m that f.seen describes.
func (f *fanout) build(db *DB, m *message, raw []byte, kept int) readable {
	switch {
	case kept == len(f.fields):
		return readable{raw: raw}
	case kept == 0 && m.Ack == "":
		return readable{none: true} // a put with nothing the peer may see
	}
	keep := graph{}
	for i, sf := range f.fields {
		if f.seen[i/8]&(1<<(i%8)) == 0 {
			continue
		}
		from := m.Put[sf.soul]
		n, ok := keep[sf.soul]
		if !ok {
			n = &Node{Soul: from.Soul, Fields: map[string]Value{}, States: map[string]float64{}}
			keep[sf.soul] = n
		}
		n.Fields[sf.field] = from.Fields[sf.field]
		if s, ok := from.States[sf.field]; ok {
			n.States[sf.field] = s
		}
	}
	cp := *m
	cp.Put, cp.Hash, cp.raw = keep, nil, nil
	if len(keep) == 0 {
		cp.Put = nil
	}
	return readable{raw: db.encode(&cp)}
}

// remoteError turns an error text received from a peer back into an error
// that matches ErrForbidden, ErrUnverified or ErrQuota.
func remoteError(text string) error {
	for _, known := range []error{ErrForbidden, ErrUnverified, ErrQuota} {
		if rest, ok := strings.CutPrefix(text, known.Error()); ok {
			return fmt.Errorf("%w%s", known, rest)
		}
	}
	return errors.New(text)
}
