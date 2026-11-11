package gundb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"ella.to/gundb/internal/js"
)

// okAck is the "ok" body of our acks, as sent by gun/src/root.js.
var okAck = json.RawMessage(`{"":1}`)

// handle processes one inbound message. The routing follows the reference
// relay (DAM + AXE):
//
//   - every message ID is de-duplicated, and its sender remembered;
//   - acks ("@") go to whoever is waiting locally, otherwise back along the
//     path the request came from;
//   - puts are merged, acked, and forwarded to peers that asked for the
//     souls that changed (and to our upstream peers);
//   - gets are answered from the store; misses are forwarded to other peers.
func (db *DB) handle(p *peer, m *message) {
	var f frameSync
	db.handleIn(p, m, &f)
	db.flush(&f)
}

// handleIn is handle for one message of a frame: what waits for the
// frame's writes to be durable is added to f (see frameSync).
func (db *DB) handleIn(p *peer, m *message, f *frameSync) {
	if m.empty() {
		return
	}
	if m.ID == "" {
		m.ID = randomID(9)
		m.raw = nil // what we pass on must carry the ID
	}
	// A handshake carrying our own PID means we dialled ourselves. Check it
	// before dedup: the message ID is one we created, so dedup would hide it.
	if m.Dam == "?" && m.PID == db.pid {
		db.log.Debug("gundb: connected to self, dropping", "peer", p.conn.RemoteAddr())
		db.peersMu.Lock()
		if p.origin != nil {
			p.origin.self = true
		}
		db.peersMu.Unlock()
		p.close()
		return
	}
	if db.dup.track(m.ID, p) {
		return
	}
	switch {
	case m.Dam != "":
		db.handleDam(p, m)
	case m.Ack != "":
		db.handleAck(p, m, f)
	default:
		if m.Put != nil {
			db.handlePut(p, m, f)
		}
		if m.Get != nil {
			db.handleGet(p, m)
		}
	}
}

func (db *DB) handleDam(p *peer, m *message) {
	switch m.Dam {
	case "?":
	case "!":
		db.log.Warn("gundb: peer reported an error", "peer", p.conn.RemoteAddr(), "err", m.errText())
		return
	default: // "hi", "opt", "mob": hints we don't need
		return
	}
	if m.PID != "" {
		p.setPID(m.PID)
		if m.Ack != "" {
			return
		}
	}
	db.reply(p, &message{Dam: "?", PID: db.pid, Ack: m.ID})
}

func (db *DB) handleAck(p *peer, m *message, f *frameSync) {
	if len(m.Put) > 0 && !p.reads() {
		m.Put, m.raw = nil, nil // a peer we only write to: its data never comes in
	}
	if len(m.Put) > 0 {
		// Data answering a get counts as a write by whoever sent it, and is
		// dropped, not relayed, if SEA rejects it. Data we pass on is
		// verified whole; data for us only where it is newer than ours
		// (see applyFrom).
		skip := checked(nil)
		err := db.allowWrite(p, m.Put)
		if err == nil && db.relaysReply(p, m) {
			err, skip = checkSEA(m.Put, nil), allChecked
		}
		if err == nil {
			if err = m.Put.check(); err != nil {
				db.log.Debug("gundb: bad reply", "peer", p.conn.RemoteAddr(), "err", err)
				err = nil // answered as found, like the reference, but not stored
			} else if _, _, err = db.applyFrom(db.ctx, m.Put, skip, !p.trusted(), f); err != nil && !errors.Is(err, ErrUnverified) {
				db.log.Warn("gundb: store failed", "err", err)
				err = nil
			}
		}
		if err != nil {
			db.log.Debug("gundb: dropped reply", "peer", p.conn.RemoteAddr(), "err", err)
			m.Put, m.raw = nil, nil
		}
	}
	if r := db.answer(p, m); r != nil {
		if r.to != nil && len(m.Put) > 0 { // data for a get we are relaying
			db.sendTo(r.to, m, db.wire(m), nil)
		}
		return
	}
	if to := db.dup.via(m.Ack); to != nil && to != p {
		db.dup.track(m.Ack, nil) // keep the route alive for chunked replies
		db.sendTo(to, m, db.wire(m), nil)
	}
}

func (db *DB) handlePut(p *peer, m *message, f *frameSync) {
	if !p.reads() {
		// A peer we only write to: its data never comes in. No ack
		// either: one relayed from another peer must not look refused.
		db.log.Debug("gundb: dropped a put from a write-only peer", "peer", p.conn.RemoteAddr())
		return
	}
	if err := m.Put.check(); err != nil {
		db.reply(p, &message{Ack: m.ID, Err: errJSON(err)})
		return
	}
	if err := db.allowWrite(p, m.Put); err != nil {
		db.reply(p, &message{Ack: m.ID, Err: errJSON(err)})
		return
	}
	changed, stale, err := db.applyFrom(db.ctx, m.Put, nil, !p.trusted(), f)
	if err != nil {
		db.reply(p, &message{Ack: m.ID, Err: errJSON(err)})
		return
	}
	// The ack, and passing the write on, wait until it is durable.
	if f.need {
		f.after = append(f.after, func(err error) { db.putDone(p, m, changed, stale, err) })
		return
	}
	db.putDone(p, m, changed, stale, nil)
}

// putDone acks the put m from p, once it is durable, and passes on what
// it changed.
func (db *DB) putDone(p *peer, m *message, changed graph, stale bool, err error) {
	if err != nil {
		db.reply(p, &message{Ack: m.ID, Err: errJSON(err)})
		return
	}
	db.reply(p, &message{Ack: m.ID, OK: okAck})
	if len(changed) == 0 {
		return
	}
	if stale {
		// Some signed fields lost HAM unverified: pass on only what won,
		// which was verified, never data that might be forged.
		cp := *m
		cp.Put, cp.Hash, cp.raw = changed, nil, nil
		m = &cp
	}
	raw := db.wire(m)
	memo := &fanout{}
	for _, q := range db.recipients(changed) {
		if q != p && !m.sentTo(q.getPID()) {
			db.sendTo(q, m, raw, memo)
		}
	}
	db.queueForAway(raw)
}

func (db *DB) handleGet(p *peer, m *message) {
	if !p.writes() {
		db.reply(p, &message{Ack: m.ID}) // a peer we only read from gets nothing
		return
	}
	q := m.Get
	if q.SoulLex != nil {
		db.handleSoulLex(p, m)
		return
	}
	if q.Soul == "" {
		db.reply(p, &message{Ack: m.ID})
		return
	}
	// A get for one field is refused up front; any other get is answered
	// with the fields p may read, so a node with none is not found.
	if q.Key != "" && q.KeyLex == nil && !db.allowRead(p, q.Soul, q.Key) {
		db.reply(p, &message{Ack: m.ID, Err: errJSON(fmt.Errorf("%w: may not read %s.%s", ErrForbidden, q.Soul, q.Key))})
		return
	}
	p.want(q.Soul)
	var n *Node
	var err error
	if fr, ok := db.store.(FieldReader); ok && q.Key != "" && q.KeyLex == nil {
		n, err = fr.GetFields(db.ctx, q.Soul, []string{q.Key}) // one field: read only it
	} else {
		n, err = db.store.Get(db.ctx, q.Soul)
	}
	if err != nil {
		db.reply(p, &message{Ack: m.ID, Err: errJSON(err)})
		return
	}
	if n != nil {
		for k := range n.Fields {
			if !q.match(k) {
				delete(n.Fields, k)
				delete(n.States, k)
			}
		}
		db.readableNode(p, n)
		if len(n.Fields) > nodeChunk {
			db.replyChunks(p, m.ID, n)
			return
		}
		if len(n.Fields) > 0 {
			db.reply(p, &message{Ack: m.ID, Put: graph{q.Soul: n}})
			return
		}
	}
	db.relayGet(p, m)
}

// nodeChunk is how many fields of a node one message carries at most: a
// list of 100,000 items is sent in 100 messages, not one frame of many
// megabytes that the reader must hold whole.
const nodeChunk = 1000

// replyChunks answers the get ack with n in chunks of nodeChunk fields,
// all but the last marked "%", each written straight from n.
func (db *DB) replyChunks(p *peer, ack string, n *Node) {
	keys := keysOf(n.Fields, false)
	for i := 0; i < len(keys); i += nodeChunk {
		part := keys[i:min(i+nodeChunk, len(keys))]
		id := randomID(9)
		db.dup.track(id, nil)
		b := append(make([]byte, 0, 64+len(part)*48), `{"#":`...)
		b = append(js.AppendQuote(b, id), `,"@":`...)
		b = append(js.AppendQuote(b, ack), `,"put":{`...)
		b = append(js.AppendQuote(b, n.Soul), ':')
		b, err := n.appendKeysJSON(b, part)
		if err != nil {
			db.log.Error("gundb: encode failed", "err", err)
			return
		}
		b = append(b, '}')
		if i+nodeChunk < len(keys) {
			b = append(b, `,"%":1`...)
		}
		p.send(append(b, '}'))
	}
}

// chunkNode splits n into nodes of at most size fields each. A node that
// fits is returned as it is.
func chunkNode(n *Node, size int) []*Node {
	if len(n.Fields) <= size {
		return []*Node{n}
	}
	parts := make([]*Node, 0, (len(n.Fields)+size-1)/size)
	var cur *Node
	for k, v := range n.Fields {
		if cur == nil || len(cur.Fields) == size {
			cur = &Node{Soul: n.Soul, Fields: make(map[string]Value, size), States: make(map[string]float64, size)}
			parts = append(parts, cur)
		}
		cur.Fields[k], cur.States[k] = v, n.States[k]
	}
	return parts
}

// lexChunk is how many nodes go in one reply to a LEX query over souls.
const lexChunk = 100

// handleSoulLex answers a get for every soul matching a LEX, like radisk
// does: the matching nodes p may read, in chunks marked "%" but the last.
// p is subscribed to each one. With no store support or no match, the get
// is passed on to other peers.
func (db *DB) handleSoulLex(p *peer, m *message) {
	l := m.Get.SoulLex
	ss, ok := db.store.(SoulStore)
	if !ok {
		db.relayGet(p, m)
		return
	}
	souls, err := listSouls(db.ctx, ss, l)
	if err != nil {
		db.reply(p, &message{Ack: m.ID, Err: errJSON(err)})
		return
	}
	// Each chunk is sent once the next match shows more are coming, so only
	// one chunk of nodes is held at a time.
	var chunk graph
	for _, soul := range souls {
		n, err := db.store.Get(db.ctx, soul)
		if err != nil {
			db.reply(p, &message{Ack: m.ID, Err: errJSON(err)})
			return
		}
		if n != nil {
			db.readableNode(p, n)
		}
		if n == nil || len(n.Fields) == 0 {
			continue
		}
		p.want(soul)
		if len(chunk) == lexChunk {
			db.reply(p, &message{Ack: m.ID, Put: chunk, More: json.RawMessage("1")})
			chunk = nil
		}
		if chunk == nil {
			chunk = graph{}
		}
		chunk[soul] = n
	}
	if chunk == nil {
		db.relayGet(p, m)
		return
	}
	db.reply(p, &message{Ack: m.ID, Put: chunk})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// relayGet asks our other peers for data we don't have. Their data is passed
// back to p as it arrives; if nobody has any, p gets a single "not found"
// once they have all answered or Options.Wait has passed.
//
// With Options.RouteByInterest only the peers we dialled, and those that
// asked for the same node, are asked; otherwise every peer we read from.
func (db *DB) relayGet(p *peer, m *message) {
	var others []*peer
	for _, o := range db.askFor(m.Get) {
		if o != p && !m.sentTo(o.getPID()) {
			others = append(others, o)
		}
	}
	if len(others) == 0 {
		db.reply(p, &message{Ack: m.ID})
		return
	}
	r := db.newRequest(m.ID, false, others)
	r.to = p
	// No goroutine waits for the answers: whichever comes first of the
	// last answer, the timeout and Close finishes the request.
	var once sync.Once
	var timer *time.Timer
	var stop func() bool
	finish := func() {
		once.Do(func() {
			db.reqMu.Lock() // timer and stop are set under it
			t, cancel := timer, stop
			db.reqMu.Unlock()
			t.Stop()
			cancel()
			db.endRequest(m.ID)
			db.reqMu.Lock()
			found := r.found
			db.reqMu.Unlock()
			if !found {
				db.reply(p, &message{Ack: m.ID})
			}
		})
	}
	db.reqMu.Lock()
	r.finish = finish
	timer = time.AfterFunc(db.wait, finish)
	stop = context.AfterFunc(db.ctx, finish)
	db.reqMu.Unlock()
	raw := db.wire(m)
	for _, o := range others {
		o.send(raw)
	}
}

// askFor returns the peers to pass on a get we cannot answer: every peer
// we read from or, with Options.RouteByInterest, the peers we dialled and
// read from, and those that asked for the same node.
func (db *DB) askFor(q *getQuery) []*peer {
	if !db.routeByInterest {
		return db.peersFor(PeerRead)
	}
	out := db.upstream(PeerRead)
	if q.Soul == "" {
		return out
	}
	db.subsMu.Lock()
	defer db.subsMu.Unlock()
	for s := range db.subs[q.Soul] {
		if s.origin == nil && !slices.Contains(out, s) { // dialled peers are in out already
			out = append(out, s)
		}
	}
	return out
}

// relaysReply reports whether the reply m from p will be passed on: it
// answers a get we relayed for someone else.
func (db *DB) relaysReply(p *peer, m *message) bool {
	db.reqMu.Lock()
	r := db.pending[m.Ack]
	db.reqMu.Unlock()
	if r != nil {
		return r.to != nil
	}
	to := db.dup.via(m.Ack)
	return to != nil && to != p
}

// reply sends a new message (with a fresh ID) to p.
func (db *DB) reply(p *peer, m *message) {
	m.ID = randomID(9)
	db.dup.track(m.ID, nil)
	p.send(db.encode(m))
}

// wire returns m as it goes on the wire: as it was received if it is
// unchanged, else encoded. A message received in a batch is copied out of
// it, so the queues it waits in don't keep the whole frame alive.
func (db *DB) wire(m *message) []byte {
	switch {
	case m.raw == nil:
		return db.encode(m)
	case cap(m.raw) > 2*len(m.raw):
		m.raw = bytes.Clone(m.raw)
	}
	return m.raw
}

func (db *DB) encode(m *message) []byte {
	raw, err := m.appendWire(nil)
	if err != nil {
		db.log.Error("gundb: encode failed", "err", err)
		return nil
	}
	return raw
}

func errJSON(err error) json.RawMessage { return json.RawMessage(jsQuote(err.Error())) }

// check rejects puts the reference would reject as an invalid graph.
func (g graph) check() error {
	for soul, n := range g {
		if n == nil {
			return fmt.Errorf("gundb: invalid graph: %q has no node", soul)
		}
		if n.Soul != soul {
			return fmt.Errorf("gundb: invalid graph: %q soul not same", soul)
		}
	}
	return nil
}
