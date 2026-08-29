package gundb

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"ella.to/gundb/transport"
)

// peer is one live connection.
type peer struct {
	db     *DB
	conn   transport.Conn
	dialed bool      // we dialled it (an upstream peer)
	origin *outbound // the configured peer this connection belongs to, if dialled
	out    chan []byte
	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	pid   string
	wants map[string]bool // souls the remote asked us for
}

// outbound is a peer we dial and keep re-dialling. Guarded by db.peersMu.
type outbound struct {
	url    string
	trying bool     // first dial attempt still in progress
	self   bool     // the URL turned out to be ourselves: stop dialling
	live   *peer    // current connection, if any
	queue  [][]byte // our own writes made while disconnected
}

// maxQueue bounds the writes buffered for a disconnected peer.
const maxQueue = 10_000

// run serves one connection until it closes.
func (db *DB) run(conn transport.Conn, o *outbound) {
	ctx, cancel := context.WithCancel(db.ctx)
	p := &peer{db: db, conn: conn, dialed: o != nil, origin: o, out: make(chan []byte, 4096), ctx: ctx, cancel: cancel, wants: map[string]bool{}}

	db.peersMu.Lock()
	if db.ctx.Err() != nil {
		db.peersMu.Unlock()
		conn.Close()
		return
	}
	db.peers[p] = struct{}{}
	var queued [][]byte
	if o != nil {
		o.live, o.trying, queued, o.queue = p, false, o.queue, nil
	}
	db.signalLocked()
	db.peersMu.Unlock()
	db.log.Debug("gundb: peer connected", "peer", conn.RemoteAddr())

	go p.writeLoop()
	if o == nil {
		// Like mesh.hi in the reference, the accepting side opens the DAM handshake.
		db.reply(p, &message{Dam: "?", PID: db.pid})
	}
	for _, raw := range queued {
		p.send(raw)
	}
	db.resubscribe(p)
	p.readLoop()

	p.close()
	db.peersMu.Lock()
	delete(db.peers, p)
	if o != nil && o.live == p {
		o.live = nil
	}
	db.signalLocked()
	db.peersMu.Unlock()
	db.peerGone(p)
	db.log.Debug("gundb: peer disconnected", "peer", conn.RemoteAddr())
}

func (p *peer) readLoop() {
	for {
		frame, err := p.conn.Recv(p.ctx)
		if err != nil {
			if !transport.IsClosed(err) && p.ctx.Err() == nil {
				p.db.log.Debug("gundb: read failed", "peer", p.conn.RemoteAddr(), "err", err)
			}
			return
		}
		msgs, err := readFrame(frame)
		if err != nil {
			p.db.log.Debug("gundb: bad frame", "peer", p.conn.RemoteAddr(), "err", err)
		}
		for i := range msgs {
			p.db.handle(p, &msgs[i])
		}
	}
}

// writeLoop sends queued messages, batching whatever has piled up into one
// JSON array frame the way the reference mesh does, plus heartbeats.
func (p *peer) writeLoop() {
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		var frame []byte
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
			frame = heartbeat
		case raw := <-p.out:
			frame = raw
			if n := min(len(p.out), 99); n > 0 {
				frame = append([]byte{'['}, raw...)
				for range n {
					frame = append(append(frame, ','), <-p.out...)
				}
				frame = append(frame, ']')
			}
		}
		ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
		err := p.conn.Send(ctx, frame)
		cancel()
		if err != nil {
			p.close()
			return
		}
	}
}

// send queues one encoded message. A peer that stays backed up for 5s is
// disconnected rather than allowed to stall the rest of the mesh.
func (p *peer) send(raw []byte) {
	if raw == nil {
		return
	}
	select {
	case p.out <- raw:
		return
	case <-p.ctx.Done():
		return
	default:
	}
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
	select {
	case p.out <- raw:
	case <-p.ctx.Done():
	case <-t.C:
		p.db.log.Warn("gundb: peer too slow, disconnecting", "peer", p.conn.RemoteAddr())
		p.close()
	}
}

func (p *peer) close() {
	p.cancel()
	p.conn.Close()
}

func (p *peer) setPID(pid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pid == "" {
		p.pid = pid
	}
}

func (p *peer) getPID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pid
}

func (p *peer) want(soul string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.wants[soul] = true
}

func (p *peer) wantsAny(g graph) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for soul := range g {
		if p.wants[soul] {
			return true
		}
	}
	return false
}

func (db *DB) livePeers() []*peer {
	db.peersMu.Lock()
	defer db.peersMu.Unlock()
	return slices.Collect(maps.Keys(db.peers))
}

// sendOwn sends a message we created to every peer, and queues it for
// configured peers that are currently disconnected.
func (db *DB) sendOwn(raw []byte) {
	db.peersMu.Lock()
	peers := slices.Collect(maps.Keys(db.peers))
	for _, o := range db.outbound {
		if o.live == nil && len(o.queue) < maxQueue {
			o.queue = append(o.queue, raw)
		}
	}
	db.peersMu.Unlock()
	for _, p := range peers {
		p.send(raw)
	}
}
