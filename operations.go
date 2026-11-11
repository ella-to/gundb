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
// The DB's own writes are counted against Options.Quota but never refused;
// applyFrom refuses a peer's write that would break it.
func (db *DB) apply(ctx context.Context, g graph, skip checked) (graph, error) {
	changed, _, err := db.applyFrom(ctx, g, skip, false, nil)
	return changed, err
}

// frameSync holds back what must wait until the writes of one frame are
// durable, so the frame needs a single SyncStore.Sync. Its zero value is
// ready to use.
type frameSync struct {
	need  bool              // a write still has to be synced
	after []func(err error) // run after the sync, with its error
}

// flush syncs the writes of a frame and runs what waited for it.
func (db *DB) flush(f *frameSync) {
	var err error
	if f.need {
		if ss, ok := db.store.(SyncStore); ok {
			if err = ss.Sync(db.ctx); err != nil {
				db.log.Warn("gundb: sync failed", "err", err)
			}
		}
		f.need = false
	}
	for _, fn := range f.after {
		fn(err)
	}
	f.after = f.after[:0]
}

// applyFrom is apply, with enforce saying whether the quota may refuse the
// write: true for data from peers that connect to us. With a SyncStore the
// write is durable when applyFrom returns, unless f is given: f.need then
// says that the caller still has to sync (see frameSync).
//
// Signatures are verified only for fields that can be stored: those newer
// than what the store holds. A field equal to the stored one was verified
// when it was stored, and an older one is dropped by HAM anyway, so a peer
// offering a node we already have costs no verification at all. stale
// reports that g had such signed fields: they were neither verified nor
// stored, so g must not be passed on as it is (pass on changed instead).
func (db *DB) applyFrom(ctx context.Context, g graph, skip checked, enforce bool, f *frameSync) (changed graph, stale bool, err error) {
	sigs, err := checkUnsigned(g, skip)
	if err != nil {
		return nil, false, err
	}
	// pending holds the signed fields not verified yet. Verify those that
	// can win now, outside the merge locks, in parallel. One that wins only
	// because the store changed meanwhile is verified during the merge.
	var pending map[string]map[string]soulKind
	if len(sigs) > 0 {
		pending = make(map[string]map[string]soulKind)
		for _, f := range sigs {
			if pending[f.soul] == nil {
				pending[f.soul] = map[string]soulKind{}
			}
			pending[f.soul][f.field] = f.kind
		}
		wins := db.mayWin(ctx, g, sigs, db.state.Next())
		if err := verifyAll(wins); err != nil {
			return nil, false, err
		}
		for _, f := range wins {
			delete(pending[f.soul], f.field)
		}
		for soul, fields := range pending {
			if len(fields) == 0 {
				delete(pending, soul)
			}
		}
		stale = len(pending) > 0
	}

	changed = graph{}
	var later graph // writes from the future, held: made when there is one
	var wake float64
	machine := db.state.Next()
	var deltas []ownerDelta // with a quota: how this write changes each owner
	// merge applies in to the node with HAM, in memory: the caller writes
	// cur (or the changed fields) to the store.
	merge := func(soul string, in *Node) (cur *Node, dirty bool, err error) {
		cur, err = db.current(ctx, soul, in)
		if err != nil {
			return nil, false, err
		}
		var olds map[string]Value
		var late []fieldRef // signed fields that won but were not verified
		unverified := pending[soul]
		for k, v := range in.Fields {
			state := in.States[k]
			curState, ok := cur.States[k]
			if !ok {
				curState = math.Inf(-1)
			}
			switch r := ham(machine, state, curState, v, cur.Fields[k]); r {
			case hamIncoming, hamDefer:
				if kind, ok := unverified[k]; ok {
					late = append(late, fieldRef{soul, k, v, state, kind})
				}
				if r == hamDefer {
					if later == nil {
						later = graph{}
					}
					later.node(soul).Set(k, v, state)
					wake = max(wake, state)
					continue
				}
				if db.quota != nil {
					if olds == nil {
						olds = map[string]Value{}
					}
					olds[k] = cur.Fields[k]
				}
				cur.Set(k, v, state)
				changed.node(soul).Set(k, v, state)
				dirty = true
			}
		}
		if err := verifyAll(late); err != nil {
			return nil, false, err
		}
		if dirty && db.quota != nil {
			if owner := db.quota.owner(soul); owner != "" {
				_, partial := db.store.(FieldReader)
				d, err := db.usageDelta(ctx, soul, in, cur, olds, !partial)
				if err != nil {
					return nil, false, err
				}
				deltas = addDelta(deltas, owner, d)
			}
		}
		return cur, dirty, nil
	}
	// With a SyncStore, writes go to memory under the merge locks, and are
	// synced after (see SyncStore).
	ss, syncs := db.store.(SyncStore)
	unsynced := false
	// store writes one merged node: only its changed fields if it can.
	store := func(soul string, cur *Node) error {
		if syncs {
			unsynced = true
			return ss.PutBatchUnsynced(ctx, []*Node{changed[soul]})
		}
		if fs, ok := db.store.(FieldStore); ok {
			return fs.PutFields(ctx, soul, changed[soul])
		}
		return db.store.Put(ctx, cur)
	}

	bs, batch := db.store.(BatchStore)
	batch = batch && len(g) > 1
	// A merge can fail on a late signature: merge every node before
	// storing any when g has several, so a failure stores nothing.
	if batch || db.quota != nil || (len(g) > 1 && len(pending) > 0) {
		// Merge every node, holding all their locks, then check the quota,
		// then commit: at once if the store can, else children first.
		unlock := db.lockMerges(g)
		order := childrenFirst(g)
		curs := make(map[string]*Node, len(g))
		for _, soul := range order {
			cur, dirty, err := merge(soul, g[soul])
			if err != nil {
				unlock()
				return nil, false, err
			}
			if dirty {
				curs[soul] = cur
			}
		}
		if db.quota != nil && len(deltas) > 0 {
			if err := db.quota.reserve(deltas, enforce); err != nil {
				unlock()
				return nil, false, err
			}
		}
		var err error
		if batch && len(curs) > 0 {
			nodes := make([]*Node, 0, len(curs))
			for _, soul := range order {
				if curs[soul] != nil {
					nodes = append(nodes, changed[soul])
				}
			}
			if syncs {
				unsynced = true
				err = ss.PutBatchUnsynced(ctx, nodes)
			} else {
				err = bs.PutBatch(ctx, nodes)
			}
		} else {
			for _, soul := range order {
				if cur := curs[soul]; cur != nil && err == nil {
					err = store(soul, cur)
				}
			}
		}
		unlock()
		if err != nil {
			if db.quota != nil {
				db.quota.release(deltas)
			}
			return nil, false, err
		}
	} else {
		for _, soul := range childrenFirst(g) {
			mu := db.mergeLock(soul)
			mu.Lock()
			cur, dirty, err := merge(soul, g[soul])
			if err == nil && dirty {
				err = store(soul, cur)
			}
			mu.Unlock()
			if err != nil {
				return nil, false, err
			}
		}
	}

	if unsynced {
		if f != nil {
			f.need = true
		} else if err := ss.Sync(ctx); err != nil {
			return nil, false, err
		}
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
	db.touchInterest(changed)
	return changed, stale, nil
}

// mayWin returns the signed fields in sigs that can win HAM against what
// the store holds now, at machine: those that would be written or held for
// later. The rest are equal to or older than the stored ones. If the store
// cannot be read, all of them are returned.
func (db *DB) mayWin(ctx context.Context, g graph, sigs []fieldRef, machine float64) []fieldRef {
	curs := make(map[string]*Node)
	wins := sigs[:0:0]
	for _, f := range sigs {
		cur, ok := curs[f.soul]
		if !ok {
			var err error
			if cur, err = db.current(ctx, f.soul, g[f.soul]); err != nil {
				return sigs
			}
			curs[f.soul] = cur
		}
		curState, ok := cur.States[f.field]
		if !ok {
			curState = math.Inf(-1)
		}
		if r := ham(machine, f.state, curState, f.v, cur.Fields[f.field]); r == hamIncoming || r == hamDefer {
			wins = append(wins, f)
		}
	}
	return wins
}

// childrenFirst orders the souls of g so that a node comes after the nodes
// in g it links to. A reader that sees a new link then also finds the node
// behind it, instead of a dangling link while the write is half applied.
func childrenFirst(g graph) []string {
	if len(g) == 1 { // nothing to order
		for soul := range g {
			return []string{soul}
		}
	}
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

// current returns the stored node a merge of in compares against: only the
// fields in touches, if the store can read just those (FieldReader).
func (db *DB) current(ctx context.Context, soul string, in *Node) (*Node, error) {
	if ms, ok := db.store.(*memoryStore); ok { // the default
		return ms.getFieldsOf(soul, in.Fields), nil
	}
	var cur *Node
	var err error
	if fr, ok := db.store.(FieldReader); ok { // PutFields writes only these back
		cur, err = fr.GetFields(ctx, soul, slices.Collect(maps.Keys(in.Fields)))
	} else {
		cur, err = db.store.Get(ctx, soul)
	}
	if cur == nil && err == nil {
		cur = NewNode(soul)
	}
	return cur, err
}

func isFieldStore(s Store) bool {
	_, ok := s.(FieldStore)
	return ok
}

// mergeLock returns the lock that serialises merges into soul.
func (db *DB) mergeLock(soul string) *sync.Mutex { return &db.mergeMu[db.mergeIndex(soul)] }

func (db *DB) mergeIndex(soul string) uint64 {
	return maphash.String(db.mergeSeed, soul) % uint64(len(db.mergeMu))
}

// lockMerges takes the merge locks of every soul in g, in one order for
// every caller so two writes can't deadlock, and returns their unlock.
func (db *DB) lockMerges(g graph) (unlock func()) {
	idx := make([]uint64, 0, len(g))
	for soul := range g {
		idx = append(idx, db.mergeIndex(soul))
	}
	slices.Sort(idx)
	idx = slices.Compact(idx) // two souls can share a lock
	for _, i := range idx {
		db.mergeMu[i].Lock()
	}
	return func() {
		for _, i := range slices.Backward(idx) {
			db.mergeMu[i].Unlock()
		}
	}
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
	if db.alone(PeerWrite) { // nobody to send it to, now or later
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
	if !db.connected(ctx, PeerWrite) {
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

// alone reports whether no peer takes part in way: none connected, and
// none configured to dial (whose writes would be queued for later).
func (db *DB) alone(way PeerMode) bool {
	db.peersMu.Lock()
	defer db.peersMu.Unlock()
	if db.flowingLocked(way) > 0 {
		return false
	}
	for _, o := range db.outbound {
		if o.flows(way) {
			return false
		}
	}
	return true
}

// request tracks the replies to one get or put we sent.
type request struct {
	put    bool
	to     *peer          // set when relaying someone else's get
	wait   map[*peer]bool // peers still expected to answer a get
	found  bool
	err    error
	done   chan struct{}
	finish func() // if set, called once done is closed, without reqMu held
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
	r := db.pending[m.Ack]
	if r == nil {
		db.reqMu.Unlock()
		return nil
	}
	select {
	case <-r.done:
		db.reqMu.Unlock()
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
	done := r.put || (r.found && last) || len(r.wait) == 0
	if done {
		close(r.done)
	}
	db.reqMu.Unlock()
	if done && r.finish != nil {
		r.finish()
	}
	return r
}

// peerGone stops requests from waiting on a peer that disconnected.
func (db *DB) peerGone(p *peer) {
	var finished []func()
	db.reqMu.Lock()
	for _, r := range db.pending {
		if r.wait[p] {
			delete(r.wait, p)
			if len(r.wait) == 0 && !r.put {
				close(r.done)
				if r.finish != nil {
					finished = append(finished, r.finish)
				}
			}
		}
	}
	db.reqMu.Unlock()
	for _, f := range finished {
		f()
	}
}

// interest records a soul, or one field of it, that we have asked peers
// for. Peers remember who asked for what and push every later change, so
// after the first answer the local copy stays current without asking
// again.
//
// used and changed are guarded by db.interestMu.
type interest struct {
	done    chan struct{}
	used    int64     // db.sweeps at the last read
	changed time.Time // last write to it, ours or a peer's
}

// interestIdle is how long an interest nobody watches is kept after its
// last read, and sweepEvery how often idle ones are dropped. A dropped one
// is asked for again on the next read.
var (
	interestIdle = 10 * time.Minute
	sweepEvery   = time.Minute
)

// offerMargin is how long before a peer left the writes are that it is
// offered again when it comes back: those may have been lost on the way
// as the connection went down.
const offerMargin = 2 * time.Minute

// handshakeWait is how long resubscribe waits for a new peer's PID before
// offering it everything.
var handshakeWait = 2 * time.Second

// maxLeft bounds db.left.
const maxLeft = 10_000

// sweepInterest drops every sweepEvery the interests nobody watches that
// were not read for interestIdle, so the list follows what is in use, not
// everything ever read.
func (db *DB) sweepInterest() {
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-db.ctx.Done():
			return
		case <-t.C:
		}
		db.watchMu.Lock()
		watched := make(map[string]bool, len(db.watchers))
		for soul := range db.watchers {
			watched[soul] = true
		}
		db.watchMu.Unlock()
		// Reads stamp an interest with the number of sweeps so far, which
		// is cheaper than reading the clock on every read.
		cutoff := db.sweeps.Add(1) - int64(interestIdle/sweepEvery)
		db.interestMu.Lock()
		for k, in := range db.interest {
			select {
			case <-in.done:
			default:
				continue // still asking
			}
			if !watched[k.soul] && in.used < cutoff {
				delete(db.interest, k)
				if k.key != "" {
					if db.fieldInterest[k.soul]--; db.fieldInterest[k.soul] <= 0 {
						delete(db.fieldInterest, k.soul)
					}
				}
			}
		}
		db.interestMu.Unlock()
	}
}

// touchInterest records that changed was written, for resubscribe.
func (db *DB) touchInterest(changed graph) {
	if len(changed) == 0 {
		return
	}
	var now time.Time
	touch := func(in *interest) {
		if now.IsZero() {
			now = time.Now()
		}
		in.changed = now
	}
	db.interestMu.Lock()
	defer db.interestMu.Unlock()
	if len(db.interest) == 0 {
		return
	}
	for soul, n := range changed {
		if in := db.interest[interestKey{soul: soul}]; in != nil {
			touch(in)
		}
		if db.fieldInterest[soul] > 0 {
			for k := range n.Fields {
				if in := db.interest[interestKey{soul, k}]; in != nil {
					touch(in)
				}
			}
		}
	}
}

// interestKey names an interest: a whole node (key "") or one field.
type interestKey struct{ soul, key string }

// sync makes sure soul has been requested from peers, waiting (at most
// db.wait) for the first answer.
func (db *DB) sync(ctx context.Context, soul string) { db.syncKey(ctx, interestKey{soul: soul}) }

// syncField is sync for one field of soul: only that field is requested,
// unless the whole node already was.
func (db *DB) syncField(ctx context.Context, soul, key string) {
	db.syncKey(ctx, interestKey{soul, key})
}

func (db *DB) syncKey(ctx context.Context, k interestKey) {
	now := db.sweeps.Load()
	db.interestMu.Lock()
	in, ok := db.interest[k]
	if !ok && k.key != "" {
		in, ok = db.interest[interestKey{soul: k.soul}] // the node covers its fields
	}
	if !ok {
		in = &interest{done: make(chan struct{})}
		db.interest[k] = in
		if k.key != "" {
			db.fieldInterest[k.soul]++
		}
		go func() {
			defer close(in.done)
			db.ask(k)
		}()
	}
	in.used = now
	db.interestMu.Unlock()
	select {
	case <-in.done:
	case <-ctx.Done():
	case <-db.ctx.Done():
	}
}

func (db *DB) ask(k interestKey) { db.query(db.ctx, &getQuery{Soul: k.soul, Key: k.key}) }

// query sends q to every peer and waits (at most db.wait) until the answers
// are in. The data that comes back is merged into the store. A get for one
// soul also offers the peers our copy of it (or of the one field asked for).
func (db *DB) query(ctx context.Context, q *getQuery) {
	ctx, cancel := context.WithTimeout(ctx, db.wait)
	defer cancel()
	if !db.connected(ctx, PeerRead) {
		return // for souls of interest, resubscribe asks once a peer connects
	}
	id := randomID(9)
	db.dup.track(id, nil)
	if q.Soul != "" {
		db.offer(interestKey{q.Soul, q.Key}, db.peersFor(PeerWrite))
	}
	peers := db.peersFor(PeerRead)
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

// resubscribe re-sends a get for every soul (or field) of interest to a new
// peer, like the reference does on "hi", so it pushes us their changes
// too. It runs on its own goroutine, so the peer is read meanwhile.
//
// It also offers the peer our copy of each (see offer), in batches. A peer
// we were connected to a moment ago, recognized by its PID once the
// handshake gives it, is offered only what changed since it left; a new
// or restarted one, which has a new PID, everything.
func (db *DB) resubscribe(p *peer) {
	db.interestMu.Lock()
	keys := make([]interestKey, 0, len(db.interest))
	for k := range db.interest {
		keys = append(keys, k)
	}
	db.interestMu.Unlock()
	if len(keys) == 0 {
		return
	}
	if p.reads() {
		for _, k := range keys {
			if p.ctx.Err() != nil {
				return
			}
			m := &message{ID: randomID(9), Get: &getQuery{Soul: k.soul, Key: k.key}}
			db.dup.track(m.ID, nil)
			p.send(db.encode(m))
		}
	}
	if !p.writes() {
		return
	}
	pid := p.waitPID(handshakeWait)
	db.interestMu.Lock()
	since, back := db.left[pid]
	if back {
		since = since.Add(-offerMargin)
		offer := keys[:0]
		for _, k := range keys {
			if in := db.interest[k]; in != nil && in.changed.After(since) {
				offer = append(offer, k)
			}
		}
		keys = offer
	}
	db.interestMu.Unlock()
	db.offerBatch(keys, p)
}

// offerBatch offers p our copies of keys, up to offerChunk nodes per put.
func (db *DB) offerBatch(keys []interestKey, p *peer) {
	g := graph{}
	send := func() {
		if len(g) == 0 {
			return
		}
		m := &message{ID: randomID(9), Put: g}
		db.dup.track(m.ID, nil)
		db.sendTo(p, m, db.encode(m), nil)
		g = graph{}
	}
	for _, k := range keys {
		if p.ctx.Err() != nil {
			return
		}
		n := db.offered(k)
		if n == nil {
			continue
		}
		if len(n.Fields) > nodeChunk { // a large node goes on its own, in chunks
			for _, part := range chunkNode(n, nodeChunk) {
				send()
				g[k.soul] = part
			}
			send()
			continue
		}
		if cur := g[k.soul]; cur != nil {
			for f, v := range n.Fields {
				cur.Set(f, v, n.States[f])
			}
		} else {
			g[k.soul] = n
		}
		if len(g) >= offerChunk {
			send()
		}
	}
	send()
}

// offerChunk is how many nodes one put of offerBatch holds.
const offerChunk = 100

// offer sends peers our copy of a node, or of one field of it. They
// already have most of it, and HAM drops what they have, but it delivers
// writes they never got: those made offline in an earlier session (the
// send queue lives in memory, so a page reload loses it, while the store
// keeps the data), or data a relay lost when it restarted.
func (db *DB) offer(k interestKey, peers []*peer) {
	if len(peers) == 0 {
		return
	}
	n := db.offered(k)
	if n == nil {
		return
	}
	for _, part := range chunkNode(n, nodeChunk) {
		m := &message{ID: randomID(9), Put: graph{k.soul: part}}
		db.dup.track(m.ID, nil)
		raw := db.encode(m)
		memo := &fanout{}
		for _, p := range peers {
			db.sendTo(p, m, raw, memo)
		}
	}
}

// offered returns our copy of the node or field k names, or nil.
func (db *DB) offered(k interestKey) *Node {
	var n *Node
	var err error
	if fr, ok := db.store.(FieldReader); ok && k.key != "" {
		n, err = fr.GetFields(db.ctx, k.soul, []string{k.key})
	} else {
		n, err = db.store.Get(db.ctx, k.soul)
	}
	if err != nil || n == nil || len(n.Fields) == 0 {
		return nil
	}
	if k.key != "" {
		for f := range n.Fields {
			if f != k.key {
				delete(n.Fields, f)
				delete(n.States, f)
			}
		}
		if len(n.Fields) == 0 {
			return nil
		}
	}
	return n
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
