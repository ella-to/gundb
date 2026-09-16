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
	// disconnected are sent once the connection is back.
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
	}
	for _, url := range o.Peers {
		db.Connect(url)
	}
	return db
}

// Get returns a reference to the root node with the given key (its soul),
// like gun.get(key) in JS.
func (db *DB) Get(key string) *Ref { return &Ref{db: db, path: []string{key}} }

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
// like the Peers option. It returns immediately.
func (db *DB) Connect(url string) {
	o := &outbound{url: url, trying: true}
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
			db.run(conn, o)
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
// until it closes. Use it to plug in any transport:
//
//	go db.Attach(conn)
func (db *DB) Attach(conn transport.Conn) { db.run(conn, nil) }

// Serve accepts peers from l until l is closed or ctx is done.
func (db *DB) Serve(ctx context.Context, l transport.Listener) error {
	for {
		conn, err := l.Accept(ctx)
		if err != nil {
			if transport.IsClosed(err) || ctx.Err() != nil || db.ctx.Err() != nil {
				return nil
			}
			return err
		}
		go db.Attach(conn)
	}
}

// ServeHTTP upgrades the request to a WebSocket and serves it as a peer.
func (db *DB) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := ws.Accept(w, r)
	if err != nil {
		db.log.Debug("gundb: websocket upgrade failed", "remote", r.RemoteAddr, "err", err)
		return
	}
	db.Attach(conn)
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
func (db *DB) connected(ctx context.Context) bool {
	for {
		db.peersMu.Lock()
		n, ch := len(db.peers), db.changed
		trying := false
		for _, o := range db.outbound {
			trying = trying || o.trying
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
