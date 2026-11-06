package gundb

import (
	"context"
	"errors"
	"hash/maphash"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"ella.to/gundb/transport"
	"ella.to/gundb/transport/ws"
)

// Options configures a DB. The zero value is a standalone in-memory database.
type Options struct {
	// Peers to connect to, like Gun({peers: [...]}) in JS. ws://, wss://,
	// http:// and https:// URLs work out of the box. Peers are re-dialled
	// every 2s when the connection drops, and writes made while
	// disconnected, this DB's own and those it relays from its clients,
	// are sent once the connection is back.
	//
	// Data flows both ways by default. The gundb_mode query parameter makes a
	// peer one way: ?gundb_mode=read only reads from it (a mirror of a primary
	// that never writes back to it), ?gundb_mode=write only writes to it (a
	// backup that is never read from). ReadFrom and WriteTo add it for
	// you. It is taken out before dialling.
	//
	//	Peers: []string{"wss://a/gun", "wss://b/gun?gundb_mode=read", gundb.WriteTo("wss://c/gun")}
	Peers []string

	// Store persists nodes. Default: NewMemoryStore().
	Store Store

	// Dialer connects to Peers. Default: WebSocket (transport/ws).
	Dialer transport.Dialer

	// Wait is how long a read waits for peers to answer before falling back
	// to local data. Default: 1s.
	Wait time.Duration

	// Logger receives diagnostics. Default: discard.
	Logger *slog.Logger

	// PID is this peer's ID in the DAM handshake. Default: random.
	PID string

	// CanRead reports whether the connection behind ctx may read field of
	// the node soul. Fields a connection may not read are never sent to it:
	// not as an answer, not as a live update, not in a soul listing, and a
	// node with no readable field is not found at all. Default: everyone may
	// read everything.
	//
	// Checking fields, not just nodes, lets a list be private while its
	// entries are not: deny every field of "users" but the reader's own, and
	// db.Get("users").Get(id) still works for that id while listing "users"
	// shows nobody else.
	//
	// ctx is the connection's context: r.Context() of the WebSocket upgrade
	// request for peers served by ServeHTTP, so a session your HTTP
	// middleware stored in it is there; or the ctx given to Attach. It is
	// the reader's context, also when another peer's write is being
	// fanned out to it.
	//
	// CanRead and CanWrite run for every field of every message, so keep
	// them fast: look the session up from ctx, don't do I/O.
	CanRead func(ctx context.Context, soul, field string) bool

	// CanWrite reports whether the connection behind ctx (see CanRead) may
	// write field of the node soul. A put with any field denied is rejected
	// whole, and PutAck on the writer returns ErrForbidden. Default:
	// everyone may write everything.
	//
	// CanRead and CanWrite apply to peers that connect to this DB. The DB's
	// own calls (db.Get(...).Put) and the peers it dials (Peers) are
	// trusted. To refuse a connection outright, refuse it in HTTP
	// middleware before it reaches the DB.
	CanWrite func(ctx context.Context, soul, field string) bool

	// OnConnect is called when a peer connects: one we dialled (Peers),
	// one that connected to us, or one handed to Attach. OnDisconnect is
	// called once that connection is gone, with the same ctx and PeerInfo.
	// ctx is the connection's context (see CanRead; for dialled peers, a
	// context that ends with the DB); by OnDisconnect it may be done, but
	// its values are still there. A peer that vanishes without closing (a
	// phone losing Wi-Fi) is noticed within about 40s.
	//
	// Both run on the connection's goroutine, so they must return quickly;
	// OnDisconnect always comes after the OnConnect it belongs to.
	OnConnect    func(ctx context.Context, info PeerInfo)
	OnDisconnect func(ctx context.Context, info PeerInfo)

	// Quota counts how many nodes and bytes each owner keeps, and refuses
	// writes from connecting peers that would take an owner past their
	// plan's limits, with ErrQuota. See Quota. Default: nothing is counted.
	Quota *Quota
}

// PeerInfo describes one connection, for Options.OnConnect and
// Options.OnDisconnect. Who is behind it is in the connection's context.
type PeerInfo struct {
	PeerID string   // unique to this connection, chosen by gundb
	URL    string   // the configured peer's URL, if this DB dialled it
	Mode   PeerMode // which way data flows with a peer this DB dialled
	Remote string   // the remote address
}

// DB is a GUN peer: a local graph that syncs with other peers in real time.
// Read and write it through Get, which returns a chainable *Ref.
//
// A DB is also an http.Handler that accepts WebSocket peers, so it can be a
// relay for JS and Go clients alike:
//
//	http.Handle("/gun", db)
type DB struct {
	pid    string
	store  Store
	dialer transport.Dialer
	wait   time.Duration
	beat   time.Duration // heartbeatEvery, read once in New
	pong   time.Duration // pongWait, read once in New
	log    *slog.Logger

	state *stateGen
	dup   *dup

	ctx    context.Context // cancelled by Close
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// mergeMu serialises read-merge-write per soul. HAM merges each node on
	// its own, so writes to different souls run in parallel (and a store
	// can sync them to disk together).
	mergeMu   [256]sync.Mutex
	mergeSeed maphash.Seed

	peersMu  sync.Mutex
	peers    map[*peer]struct{}
	outbound []*outbound
	changed  chan struct{} // closed and replaced whenever connectivity changes

	reqMu   sync.Mutex
	pending map[string]*request

	interestMu sync.Mutex
	interest   map[string]*interest

	watchMu  sync.Mutex
	watchers map[string]map[*watcher]struct{}

	subsMu sync.Mutex
	subs   map[string]map[*peer]struct{} // soul -> peers that asked for it

	canRead      func(ctx context.Context, soul, field string) bool
	canWrite     func(ctx context.Context, soul, field string) bool
	onConnect    func(context.Context, PeerInfo)
	onDisconnect func(context.Context, PeerInfo)
	quota        *quota // nil without Options.Quota
}

// Errors returned by DB and Ref methods.
var (
	ErrNotFound = errors.New("gundb: not found")
	ErrClosed   = errors.New("gundb: closed")
	ErrNoPeers  = errors.New("gundb: no peers to acknowledge the write")
)

// heartbeatEvery matches the 20s heartbeat of gun/lib/wire.js.
var heartbeatEvery = 20 * time.Second

// redialEvery matches the 2s reconnect delay of gun/src/websocket.js.
var redialEvery = 2 * time.Second

// pongWait is how long a peer has to answer a ping before its connection
// is considered dead. Without it, a peer that vanishes without closing the
// connection would look connected until TCP gives up, many minutes later.
var pongWait = 20 * time.Second

// New creates a DB. With no options it is a standalone in-memory database:
//
//	db := gundb.New()
//	db := gundb.New(gundb.Options{Peers: []string{"ws://localhost:8765/gun"}})
func New(opts ...Options) *DB {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Store == nil {
		o.Store = NewMemoryStore()
	}
	if o.Dialer == nil {
		o.Dialer = ws.Dialer{}
	}
	if o.Wait <= 0 {
		o.Wait = time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.PID == "" {
		o.PID = randomID(9)
	}
	ctx, cancel := context.WithCancel(context.Background())
	db := &DB{
		pid:       o.PID,
		store:     o.Store,
		dialer:    o.Dialer,
		wait:      o.Wait,
		beat:      heartbeatEvery,
		pong:      pongWait,
		log:       o.Logger,
		state:     newStateGen(),
		dup:       newDup(0),
		ctx:       ctx,
		cancel:    cancel,
		peers:     map[*peer]struct{}{},
		changed:   make(chan struct{}),
		pending:   map[string]*request{},
		interest:  map[string]*interest{},
		watchers:  map[string]map[*watcher]struct{}{},
		mergeSeed: maphash.MakeSeed(),
		subs:      map[string]map[*peer]struct{}{},

		canRead:      o.CanRead,
		canWrite:     o.CanWrite,
		onConnect:    o.OnConnect,
		onDisconnect: o.OnDisconnect,
	}
	if o.Quota != nil {
		// Count what is already stored before any peer can write.
		db.quota = newQuota(o.Quota)
		if err := db.quota.load(ctx, db.store); err != nil {
			db.log.Warn("gundb: quota: cannot count stored data", "err", err)
		}
	}
	for _, url := range o.Peers {
		db.Connect(url)
	}
	return db
}

// Get returns a reference to the root node with the given key (its soul),
// like gun.get(key) in JS.
func (db *DB) Get(key string) *Ref { return &Ref{db: db, path: []string{key}} }

func (db *DB) Path(path ...string) *Ref { return &Ref{db: db, path: path} }

// PID returns this peer's ID.
func (db *DB) PID() string { return db.pid }

// Peers returns the remote address of every connected peer.
func (db *DB) Peers() []string {
	db.peersMu.Lock()
	defer db.peersMu.Unlock()
	out := make([]string, 0, len(db.peers))
	for p := range db.peers {
		out = append(out, p.conn.RemoteAddr())
	}
	return out
}

// Close disconnects all peers and stops all subscriptions.
func (db *DB) Close() error {
	db.cancel()
	db.peersMu.Lock()
	for p := range db.peers {
		p.close()
	}
	db.peersMu.Unlock()
	db.wg.Wait()
	return nil
}

// Connect adds a peer by URL and keeps it connected until Close, exactly
// like the Peers option, ?gundb_mode=read and ?gundb_mode=write included (see
// ParsePeer). A URL with a bad mode is not dialled; the error is logged.
// It returns immediately.
func (db *DB) Connect(url string) {
	url, mode, err := ParsePeer(url)
	if err != nil {
		// Not dialled at all: a typo must not make a peer meant to be
		// read only one we write to.
		db.log.Error("gundb: not connecting to a peer with a bad mode", "err", err)
		return
	}
	o := &outbound{url: url, mode: mode, trying: true}
	db.peersMu.Lock()
	db.outbound = append(db.outbound, o)
	db.peersMu.Unlock()
	db.wg.Go(func() { db.dialLoop(o) })
}

func (db *DB) dialLoop(o *outbound) {
	for {
		ctx, cancel := context.WithTimeout(db.ctx, 10*time.Second)
		conn, err := db.dialer.Dial(ctx, o.url)
		cancel()
		if err != nil {
			db.log.Debug("gundb: dial failed", "peer", o.url, "err", err)
			db.peersMu.Lock()
			o.trying = false
			db.signalLocked()
			db.peersMu.Unlock()
		} else {
			db.run(db.ctx, conn, o)
		}
		db.peersMu.Lock()
		self := o.self
		db.peersMu.Unlock()
		if self {
			return
		}
		select {
		case <-db.ctx.Done():
			return
		case <-time.After(redialEvery):
		}
	}
}

// Attach runs the GUN protocol over an established connection and blocks
// until it closes, ctx is done or the DB is closed. Use it to plug in any
// transport:
//
//	go db.Attach(ctx, conn)
//
// ctx is the connection's context: Options.CanRead, CanWrite, OnConnect
// and OnDisconnect get it, so store whoever you identified behind conn in
// it.
func (db *DB) Attach(ctx context.Context, conn transport.Conn) { db.run(ctx, conn, nil) }

// Serve accepts peers from l until l is closed or ctx is done. Each peer
// is attached with ctx as its context.
func (db *DB) Serve(ctx context.Context, l transport.Listener) error {
	for {
		conn, err := l.Accept(ctx)
		if err != nil {
			if transport.IsClosed(err) || ctx.Err() != nil || db.ctx.Err() != nil {
				return nil
			}
			return err
		}
		go db.Attach(ctx, conn)
	}
}

// ServeHTTP upgrades the request to a WebSocket and serves it as a peer
// until the connection closes. r.Context() becomes the connection's
// context (see Options.CanRead), so authenticate in middleware in front
// of the DB: refuse the request there, or store the session in the
// request's context.
func (db *DB) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := ws.Accept(w, r)
	if err != nil {
		db.log.Debug("gundb: websocket upgrade failed", "remote", r.RemoteAddr, "err", err)
		return
	}
	db.run(r.Context(), conn, nil)
}

// signalLocked wakes everyone waiting for connectivity to change.
// Callers hold peersMu.
func (db *DB) signalLocked() {
	close(db.changed)
	db.changed = make(chan struct{})
}

// connected waits until at least one peer is connected. It returns false
// straight away when there is no peer to wait for, i.e. nothing connected
// and no configured peer still on its first dial attempt.
func (db *DB) connected(ctx context.Context, way PeerMode) bool {
	for {
		db.peersMu.Lock()
		n, ch := 0, db.changed
		for p := range db.peers {
			if p.flows(way) {
				n++
			}
		}
		trying := false
		for _, o := range db.outbound {
			trying = trying || (o.trying && o.flows(way))
		}
		db.peersMu.Unlock()
		if n > 0 {
			return true
		}
		if !trying {
			return false
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}
