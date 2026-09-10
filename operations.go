package gundb

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"
)

// apply merges g into the store with HAM, wakes subscribers of what changed
// and returns the accepted writes. Writes stamped in the future are held and
// retried when their time comes, like the reference does.
func (db *DB) apply(ctx context.Context, g graph) (graph, error) {
	changed, later := graph{}, graph{}
	var wake float64

	db.mergeMu.Lock()
	machine := db.state.Next()
	for soul, in := range g {
		cur, err := db.store.Get(ctx, soul)
		if err != nil {
			db.mergeMu.Unlock()
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
				db.mergeMu.Unlock()
				return nil, err
			}
		}
	}
	db.mergeMu.Unlock()

	if len(later) > 0 {
		wait := time.Duration(min(wake-machine, math.MaxInt32)+1) * time.Millisecond
		time.AfterFunc(wait, func() {
			if db.ctx.Err() == nil {
				db.apply(db.ctx, later)
			}
		})
	}
	db.notify(changed)
	return changed, nil
}

// write applies a local write and sends it to peers. With ack it waits for
// the first peer to acknowledge it.
func (db *DB) write(ctx context.Context, g graph, ack bool) error {
	if db.ctx.Err() != nil {
		return ErrClosed
	}
	if _, err := db.apply(ctx, g); err != nil {
		return err
	}
	id := randomID(9)
	db.dup.track(id, nil)
	var r *request
	if ack {
		r = db.newRequest(id, true, nil)
		defer db.endRequest(id)
	}
	db.sendOwn(db.encode(&message{ID: id, Put: g}))
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
		r.err = errors.New(e)
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

func (db *DB) ask(soul string) {
	ctx, cancel := context.WithTimeout(db.ctx, db.wait)
	defer cancel()
	if !db.connected(ctx) {
		return // resubscribe asks once a peer connects
	}
	id := randomID(9)
	db.dup.track(id, nil)
	peers := db.livePeers()
	r := db.newRequest(id, false, peers)
	defer db.endRequest(id)
	raw := db.encode(&message{ID: id, Get: &getQuery{Soul: soul}})
	for _, p := range peers {
		p.send(raw)
	}
	select {
	case <-r.done:
	case <-ctx.Done():
	}
}

// resubscribe re-sends a get for every soul of interest to a new peer, like
// the reference does on "hi", so it pushes us their changes too.
func (db *DB) resubscribe(p *peer) {
	db.interestMu.Lock()
	souls := make([]string, 0, len(db.interest))
	for soul := range db.interest {
		souls = append(souls, soul)
	}
	db.interestMu.Unlock()
	for _, soul := range souls {
		m := &message{ID: randomID(9), Get: &getQuery{Soul: soul}}
		db.dup.track(m.ID, nil)
		p.send(db.encode(m))
	}
}

// watcher re-runs a computation whenever any node it read changes.
type watcher struct {
	db      *DB
	compute func(*reader)
	poke    chan struct{}
	stop    chan struct{}
	once    sync.Once
	souls   map[string]bool
}

// watch runs compute now and again after every change to a node it read.
func (db *DB) watch(compute func(*reader)) (off func()) {
	w := &watcher{db: db, compute: compute, poke: make(chan struct{}, 1), stop: make(chan struct{}), souls: map[string]bool{}}
	w.wake()
	go w.loop()
	return func() { w.once.Do(func() { close(w.stop) }) }
}

func (w *watcher) wake() {
	select {
	case w.poke <- struct{}{}:
	default:
	}
}

func (w *watcher) loop() {
	defer w.db.rewatch(w, nil)
	for {
		select {
		case <-w.stop:
			return
		case <-w.db.ctx.Done():
			return
		case <-w.poke:
		}
		r := &reader{db: w.db, ctx: w.db.ctx, seen: map[string]bool{}}
		w.compute(r)
		if added := w.db.rewatch(w, r.seen); len(added) > 0 {
			for _, soul := range added {
				go w.db.sync(w.db.ctx, soul)
			}
			w.wake() // a change may have landed between reading and registering
		}
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

func (db *DB) notify(changed graph) {
	if len(changed) == 0 {
		return
	}
	db.watchMu.Lock()
	defer db.watchMu.Unlock()
	for soul := range changed {
		for w := range db.watchers[soul] {
			w.wake()
		}
	}
}
