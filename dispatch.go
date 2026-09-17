package gundb

import (
	"encoding/json"
	"fmt"
	"time"
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
	if m.empty() {
		return
	}
	if m.ID == "" {
		m.ID = randomID(9)
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
		db.handleAck(p, m)
	default:
		if m.Put != nil {
			db.handlePut(p, m)
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

func (db *DB) handleAck(p *peer, m *message) {
	if len(m.Put) > 0 {
		if err := m.Put.check(); err != nil {
			db.log.Debug("gundb: bad reply", "peer", p.conn.RemoteAddr(), "err", err)
		} else if _, err := db.apply(db.ctx, m.Put); err != nil {
			db.log.Warn("gundb: store failed", "err", err)
		}
	}
	if r := db.answer(p, m); r != nil {
		if r.to != nil && len(m.Put) > 0 { // data for a get we are relaying
			r.to.send(db.encode(m))
		}
		return
	}
	if to := db.dup.via(m.Ack); to != nil && to != p {
		db.dup.track(m.Ack, nil) // keep the route alive for chunked replies
		to.send(db.encode(m))
	}
}

func (db *DB) handlePut(p *peer, m *message) {
	if err := m.Put.check(); err != nil {
		db.reply(p, &message{Ack: m.ID, Err: errJSON(err)})
		return
	}
	changed, err := db.apply(db.ctx, m.Put)
	if err != nil {
		db.reply(p, &message{Ack: m.ID, Err: errJSON(err)})
		return
	}
	db.reply(p, &message{Ack: m.ID, OK: okAck})
	if len(changed) == 0 {
		return
	}
	raw := db.encode(m)
	for _, q := range db.recipients(changed) {
		if q != p && !m.sentTo(q.getPID()) {
			q.send(raw)
		}
	}
}

func (db *DB) handleGet(p *peer, m *message) {
	q := m.Get
	if q.Soul == "" { // LEX queries over souls are not supported
		db.reply(p, &message{Ack: m.ID})
		return
	}
	p.want(q.Soul)
	n, err := db.store.Get(db.ctx, q.Soul)
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
		if len(n.Fields) > 0 {
			db.reply(p, &message{Ack: m.ID, Put: graph{q.Soul: n}})
			return
		}
	}
	db.relayGet(p, m)
}

// relayGet asks our other peers for data we don't have. Their data is passed
// back to p as it arrives; if nobody has any, p gets a single "not found"
// once they have all answered or Options.Wait has passed.
func (db *DB) relayGet(p *peer, m *message) {
	var others []*peer
	for _, o := range db.livePeers() {
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
	raw := db.encode(m)
	for _, o := range others {
		o.send(raw)
	}
	go func() {
		t := time.NewTimer(db.wait)
		defer t.Stop()
		select {
		case <-r.done:
		case <-t.C:
		case <-db.ctx.Done():
		}
		db.endRequest(m.ID)
		db.reqMu.Lock()
		found := r.found
		db.reqMu.Unlock()
		if !found {
			db.reply(p, &message{Ack: m.ID})
		}
	}()
}

// reply sends a new message (with a fresh ID) to p.
func (db *DB) reply(p *peer, m *message) {
	m.ID = randomID(9)
	db.dup.track(m.ID, nil)
	p.send(db.encode(m))
}

func (db *DB) encode(m *message) []byte {
	raw, err := marshalWire(m)
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
