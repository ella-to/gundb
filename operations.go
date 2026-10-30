package gundb

import (
	"context"
	"hash/maphash"
	"maps"
	"math"
	"slices"
	"sync"
	"time"
)

// apply merges g into the store with HAM, wakes subscribers of what changed
// and returns the accepted writes. Writes stamped in the future are held and
// retried when their time comes, like the reference does.
//
// skip lists fields that need no SEA check (see checked); nil checks all.
func (db *DB) apply(ctx context.Context, g graph, skip checked) (graph, error) {
	if err := checkSEA(g, skip); err != nil {
		return nil, err
	}
	changed, later := graph{}, graph{}
	var wake float64

	machine := db.state.Next()
	fr, partial := db.store.(FieldReader)
	for _, soul := range childrenFirst(g) {
		in := g[soul]
		mu := db.mergeLock(soul)
		mu.Lock()
		var cur *Node
		var err error
		if partial { // only the fields we merge; PutFields writes only those
			cur, err = fr.GetFields(ctx, soul, slices.Collect(maps.Keys(in.Fields)))
		} else {
			cur, err = db.store.Get(ctx, soul)
		}
		if err != nil {
			mu.Unlock()
			return nil, err
		}
		if cur == nil {
			cur = NewNode(soul)
		}
		dirty := false
		for k, v := range in.Fields {
			state := in.States[k]
			curState, ok := cur.States[k]
			if !ok {
				curState = math.Inf(-1)
			}
			switch ham(machine, state, curState, v, cur.Fields[k]) {
			case hamIncoming:
				cur.Set(k, v, state)
				changed.node(soul).Set(k, v, state)
				dirty = true
			case hamDefer:
				later.node(soul).Set(k, v, state)
				wake = max(wake, state)
			}
		}
		if dirty {
			var err error
			if fs, ok := db.store.(FieldStore); ok {
				err = fs.PutFields(ctx, soul, changed[soul])
			} else {
				err = db.store.Put(ctx, cur)
			}
			if err != nil {
				mu.Unlock()
				return nil, err
			}
		}
		mu.Unlock()
	}

	if len(later) > 0 {
		wait := time.Duration(min(wake-machine, math.MaxInt32)+1) * time.Millisecond
		time.AfterFunc(wait, func() {
			if db.ctx.Err() == nil {
				db.apply(db.ctx, later, allChecked) // checked when it arrived
			}
		})
	}
	db.notify(changed)
	return changed, nil
}

// childrenFirst orders the souls of g so that a node comes after the nodes
// in g it links to. A reader that sees a new link then also finds the node
// behind it, instead of a dangling link while the write is half applied.
func childrenFirst(g graph) []string {
	order := make([]string, 0, len(g))
	done := make(map[string]bool, len(g))
	var visit func(soul string)
	visit = func(soul string) {
		if done[soul] {
			return
		}
		done[soul] = true // set first, so cycles end here
		for _, v := range g[soul].Fields {
			if l, ok := v.(Link); ok && g[l.Soul] != nil {
				visit(l.Soul)
			}
		}
		order = append(order, soul)
	}
	for soul := range g {
		visit(soul)
	}
	return order
}

// mergeLock returns the lock that serialises merges into soul.
func (db *DB) mergeLock(soul string) *sync.Mutex {
	return &db.mergeMu[maphash.String(db.mergeSeed, soul)%uint64(len(db.mergeMu))]
}

// write applies a local write and sends it to peers. With ack it waits for
// the first peer to acknowledge it. signed lists the fields we signed
// ourselves, which need no SEA check (nil if none).
func (db *DB) write(ctx context.Context, g graph, ack bool, signed checked) error {
	if db.ctx.Err() != nil {
		return ErrClosed
	}
	if _, err := db.apply(ctx, g, signed); err != nil {
		return err
	}
	if db.alone() { // nobody to send it to, now or later
		if ack {
			return ErrNoPeers
		}
		return nil
	}
	id := randomID(9)
	db.dup.track(id, nil)
	var r *request
	if ack {
		r = db.newRequest(id, true, nil)
		defer db.endRequest(id)
	}
	m := &message{ID: id, Put: g}
	db.sendOwn(m, db.encode(m))
	if !ack {
		return nil
	}
	if !db.connected(ctx) {
		return ErrNoPeers
	}
	select {
	case <-r.done:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	case <-db.ctx.Done():
		return ErrClosed
	}
}

// alone reports whether there is no peer at all: none connected, and none
// configured to dial (whose writes would be queued for later).
func (db *DB) alone() bool {
	db.peersMu.Lock()
	defer db.peersMu.Unlock()
	return len(db.peers) == 0 && len(db.outbound) == 0
}

// request tracks the replies to one get or put we sent.
type request struct {
	put   bool
	to    *peer          // set when relaying someone else's get
	wait  map[*peer]bool // peers still expected to answer a get
	found bool
	err   error
	done  chan struct{}
}

func (db *DB) newRequest(id string, put bool, peers []*peer) *request {
	r := &request{put: put, wait: map[*peer]bool{}, done: make(chan struct{})}
	for _, p := range peers {
		r.wait[p] = true
	}
	db.reqMu.Lock()
	db.pending[id] = r
	db.reqMu.Unlock()
	return r
}

func (db *DB) endRequest(id string) {
	db.reqMu.Lock()
	delete(db.pending, id)
	db.reqMu.Unlock()
}

// answer delivers an ack to a pending request and returns it (nil if the ack
// is not for one). A get is complete once a peer has sent all its data
// (replies come in chunks, all but the last marked with "%"), or once every
// peer has answered.
func (db *DB) answer(p *peer, m *message) *request {
	db.reqMu.Lock()
	defer db.reqMu.Unlock()
	r := db.pending[m.Ack]
	if r == nil {
		return nil
	}
	select {
	case <-r.done:
		return r
	default:
	}
	if e := m.errText(); e != "" {
		r.err = remoteError(e)
	}
	last := len(m.More) == 0
	if last {
		delete(r.wait, p)
	}
	r.found = r.found || len(m.Put) > 0
	if r.put || (r.found && last) || len(r.wait) == 0 {
		close(r.done)
	}
	return r
}

// peerGone stops requests from waiting on a peer that disconnected.
func (db *DB) peerGone(p *peer) {
	db.reqMu.Lock()
	defer db.reqMu.Unlock()
	for _, r := range db.pending {
		if r.wait[p] {
			delete(r.wait, p)
			if len(r.wait) == 0 && !r.put {
				close(r.done)
			}
		}
	}
}

// interest records a soul we have asked peers for. Peers remember who
// asked for what and push every later change, so after the first answer
// the local copy stays current without asking again.
type interest struct{ done chan struct{} }

// sync makes sure soul has been requested from peers, waiting (at most
// db.wait) for the first answer.
func (db *DB) sync(ctx context.Context, soul string) {
	db.interestMu.Lock()
	in, ok := db.interest[soul]
	if !ok {
		in = &interest{done: make(chan struct{})}
		db.interest[soul] = in
		go func() {
			defer close(in.done)
			db.ask(soul)
		}()
	}
	db.interestMu.Unlock()
	select {
	case <-in.done:
	case <-ctx.Done():
	case <-db.ctx.Done():
	}
}

func (db *DB) ask(soul string) { db.query(db.ctx, &getQuery{Soul: soul}) }

// query sends q to every peer and waits (at most db.wait) until the answers
// are in. The data that comes back is merged into the store. A get for one
// soul also offers the peers our copy of it.
func (db *DB) query(ctx context.Context, q *getQuery) {
	ctx, cancel := context.WithTimeout(ctx, db.wait)
	defer cancel()
	if !db.connected(ctx) {
		return // for souls of interest, resubscribe asks once a peer connects
	}
	id := randomID(9)
	db.dup.track(id, nil)
	peers := db.livePeers()
	if q.Soul != "" {
		db.offer(q.Soul, peers)
	}
	r := db.newRequest(id, false, peers)
	defer db.endRequest(id)
	raw := db.encode(&message{ID: id, Get: q})
	for _, p := range peers {
		p.send(raw)
	}
	select {
	case <-r.done:
	case <-ctx.Done():
	}
}

// resubscribe re-sends a get for every soul of interest to a new peer, like
// the reference does on "hi", so it pushes us their changes too. It offers
// our copy of each as well.
func (db *DB) resubscribe(p *peer) {
	db.interestMu.Lock()
	souls := make([]string, 0, len(db.interest))
	for soul := range db.interest {
		souls = append(souls, soul)
	}
	db.interestMu.Unlock()
	for _, soul := range souls {
		db.offer(soul, []*peer{p})
		m := &message{ID: randomID(9), Get: &getQuery{Soul: soul}}
		db.dup.track(m.ID, nil)
		p.send(db.encode(m))
	}
}

// offer sends peers our copy of soul. They already have most of it, and HAM
// drops what they have, but it delivers writes they never got: those made
// offline in an earlier session (the send queue lives in memory, so a page
// reload loses it, while the store keeps the data), or data a relay lost
// when it restarted.
func (db *DB) offer(soul string, peers []*peer) {
	n, err := db.store.Get(db.ctx, soul)
	if err != nil || n == nil || len(n.Fields) == 0 || len(peers) == 0 {
		return
	}
	m := &message{ID: randomID(9), Put: graph{soul: n}}
	db.dup.track(m.ID, nil)
	raw := db.encode(m)
	for _, p := range peers {
		db.sendTo(p, m, raw)
	}
}

// watcher re-runs a computation whenever any node it read changes.
type watcher struct {
	db      *DB
	compute func(*reader) (emit func())
	poke    chan struct{}
	stop    chan struct{}
	once    sync.Once
	souls   map[string]bool

	dirtyMu sync.Mutex
	dirty   map[string]bool // souls changed since the last run
	all     bool            // the next run recomputes everything (the first one)
}

// watch runs compute now and again after every change to a node it read.
// compute only reads; the emit func it returns runs the user's callbacks on
// the watcher's goroutine. The first read happens before watch returns, so
// it sees exactly the data present when the subscription was made.
func (db *DB) watch(compute func(*reader) (emit func())) (off func()) {
	w := &watcher{db: db, compute: compute, poke: make(chan struct{}, 1), stop: make(chan struct{}), souls: map[string]bool{}, all: true}
	emit := w.run()
	go w.loop(emit)
	return func() { w.once.Do(func() { close(w.stop) }) }
}

func (w *watcher) wake() {
	select {
	case w.poke <- struct{}{}:
	default:
	}
}

// mark records that souls changed and wakes the watcher.
func (w *watcher) mark(souls ...string) {
	w.dirtyMu.Lock()
	if !w.all {
		if w.dirty == nil {
			w.dirty = make(map[string]bool, len(souls))
		}
		for _, soul := range souls {
			w.dirty[soul] = true
		}
	}
	w.dirtyMu.Unlock()
	w.wake()
}

// take returns the souls changed since the last run and starts a new set.
// nil means everything, as on the first run.
func (w *watcher) take() map[string]bool {
	w.dirtyMu.Lock()
	defer w.dirtyMu.Unlock()
	if w.all {
		w.all, w.dirty = false, nil
		return nil
	}
	d := w.dirty
	w.dirty = nil
	if d == nil {
		d = map[string]bool{}
	}
	return d
}

// run reads once and starts watching the souls it read. A compute that
// tracks its souls itself (r.delta) only reports the ones that changed.
func (w *watcher) run() (emit func()) {
	r := &reader{db: w.db, ctx: w.db.ctx, seen: map[string]bool{}, changed: w.take()}
	emit = w.compute(r)
	var added []string
	if r.delta != nil {
		added = w.db.watchDelta(w, r.delta)
	} else {
		added = w.db.rewatch(w, r.seen)
	}
	if len(added) > 0 {
		for _, soul := range added {
			go w.db.sync(w.db.ctx, soul)
		}
		w.mark(added...) // a change may have landed between reading and registering
	}
	return emit
}

func (w *watcher) loop(emit func()) {
	defer w.db.rewatch(w, nil)
	for {
		if emit != nil {
			select {
			case <-w.stop:
				return
			default:
			}
			emit()
		}
		select {
		case <-w.stop:
			return
		case <-w.db.ctx.Done():
			return
		case <-w.poke:
		}
		emit = w.run()
	}
}

// rewatch points w at a new set of souls and returns the ones that are new.
func (db *DB) rewatch(w *watcher, souls map[string]bool) []string {
	db.watchMu.Lock()
	defer db.watchMu.Unlock()
	var added []string
	for soul := range w.souls {
		if !souls[soul] {
			delete(db.watchers[soul], w)
			if len(db.watchers[soul]) == 0 {
				delete(db.watchers, soul)
			}
		}
	}
	for soul := range souls {
		if !w.souls[soul] {
			if db.watchers[soul] == nil {
				db.watchers[soul] = map[*watcher]struct{}{}
			}
			db.watchers[soul][w] = struct{}{}
			added = append(added, soul)
		}
	}
	w.souls = souls
	return added
}

// watchDelta starts (true) or stops (false) watching each soul in delta
// for w, and returns the souls that are new.
func (db *DB) watchDelta(w *watcher, delta map[string]bool) []string {
	db.watchMu.Lock()
	defer db.watchMu.Unlock()
	var added []string
	for soul, watch := range delta {
		switch {
		case watch && !w.souls[soul]:
			if db.watchers[soul] == nil {
				db.watchers[soul] = map[*watcher]struct{}{}
			}
			db.watchers[soul][w] = struct{}{}
			w.souls[soul] = true
			added = append(added, soul)
		case !watch && w.souls[soul]:
			delete(w.souls, soul)
			delete(db.watchers[soul], w)
			if len(db.watchers[soul]) == 0 {
				delete(db.watchers, soul)
			}
		}
	}
	return added
}

func (db *DB) notify(changed graph) {
	if len(changed) == 0 {
		return
	}
	db.watchMu.Lock()
	defer db.watchMu.Unlock()
	for soul := range changed {
		for w := range db.watchers[soul] {
			w.mark(soul)
		}
	}
}
