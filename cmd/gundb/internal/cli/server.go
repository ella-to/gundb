package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	ucli "github.com/urfave/cli/v3"

	"ella.to/gundb"
	"ella.to/gundb/storage/pebblestore"
)

// defaultPort is GUN's usual port.
const defaultPort = "8765"

func (a *App) serverCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "server",
		Usage: "run a GUN relay that Go and GUN.js peers connect to",
		Description: `Serves GUN over WebSocket at --addr until Ctrl-C.

  gundb server                                   # http://localhost:8765/gun, in memory
  gundb server --addr http://0.0.0.0:8080/gun    # every interface, port 8080
  gundb server --peers http://localhost:8081/gun,http://localhost:8082/gun
  gundb server --store pebble --data ./data      # keep the data on disk

Browsers connect with Gun(['http://localhost:8765/gun']).`,
		Flags: []ucli.Flag{
			&ucli.StringFlag{
				Name:    "addr",
				Aliases: []string{"a"},
				Usage:   "`URL` to serve on; the host and port to listen on, the path to serve GUN at",
				Value:   DefaultAddr,
				Sources: ucli.EnvVars("GUNDB_ADDR"),
			},
			&ucli.StringSliceFlag{
				Name:    "peers",
				Usage:   "other relays to sync with, as comma-separated `URLs`; repeat it for more",
				Sources: ucli.EnvVars("GUNDB_PEERS"),
			},
			&ucli.DurationFlag{
				Name:    "timeout",
				Aliases: []string{"t"},
				Usage:   "how long a read waits for --peers to answer before using local data; 0 waits for every peer",
				Value:   defaultTimeout,
				Sources: ucli.EnvVars("GUNDB_TIMEOUT"),
			},
			&ucli.StringFlag{
				Name:    "store",
				Usage:   "where to keep the data: memory (lost on exit) or pebble (on disk, in --data)",
				Value:   "memory",
				Sources: ucli.EnvVars("GUNDB_STORE"),
			},
			&ucli.StringFlag{
				Name:    "data",
				Usage:   "`directory` for --store pebble; setting it implies --store pebble",
				Value:   "gundb-data",
				Sources: ucli.EnvVars("GUNDB_DATA"),
			},
			&ucli.BoolFlag{
				Name:  "no-sync",
				Usage: "with --store pebble, skip the fsync after each write: faster, but a crash can lose the last writes",
			},
		},
		OnUsageError: usageError,
		Action:       a.serve,
	}
}

func (a *App) serve(ctx context.Context, c *ucli.Command) error {
	if c.Args().Len() > 0 {
		return fail("give the address with --addr, e.g. gundb server --addr "+DefaultAddr,
			"gundb server takes no arguments, got %q", c.Args().Slice())
	}
	log, lib := a.logger(c)

	host, path, err := listenAddr(c.String("addr"))
	if err != nil {
		return err
	}
	peers, err := peerList(c.StringSlice("peers"))
	if err != nil {
		return err
	}
	store, closeStore, err := openStore(c, log)
	if err != nil {
		return err
	}
	defer closeStore()

	wait := c.Duration("timeout")
	if wait == 0 {
		wait = 24 * time.Hour // "forever": every peer answers or gives up long before
	}
	db := gundb.New(gundb.Options{
		Peers:        peers,
		Store:        store,
		Dialer:       peerDialer(log),
		Wait:         wait,
		Logger:       lib,
		OnConnect:    func(_ context.Context, p gundb.PeerInfo) { logPeer(log, p, true, false) },
		OnDisconnect: func(_ context.Context, p gundb.PeerInfo) { logPeer(log, p, false, ctx.Err() != nil) },
	})
	defer db.Close()

	ln, err := net.Listen("tcp", host)
	if err != nil {
		return explainListen(host, err)
	}
	mux := http.NewServeMux()
	mux.Handle(path, db)
	if path != "/" {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, "gundb relay: connect GUN peers to %s\n", path)
		})
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ErrorLog: slog.NewLogLogger(lib.Handler(), slog.LevelDebug)}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	addr := publicAddr(c.String("addr"), ln.Addr().(*net.TCPAddr), path)
	log.Info("gundb server is running", "addr", addr)
	if len(peers) > 0 {
		log.Info("syncing with peers", "peers", strings.Join(peers, ","))
	}
	if a.onListen != nil {
		a.onListen(addr)
	}

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-served:
		return fmt.Errorf("the server stopped: %w", err)
	}
	db.Close() // closes the WebSocket connections, which Shutdown would wait for
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		log.Warn("some connections did not close in time", "err", err.Error())
	}
	return nil
}

// listenAddr splits --addr into the host:port to listen on and the path
// to serve GUN at. The scheme may be left out: localhost:8080 works.
func listenAddr(raw string) (host, path string, err error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fail("use a URL such as "+DefaultAddr, "bad --addr %q: %v", raw, err)
	}
	switch u.Scheme {
	case "http", "ws":
	case "https", "wss":
		return "", "", fail("serve plain http and put a TLS proxy (caddy, nginx) in front for https",
			"gundb server does not do TLS itself, got %q", raw)
	default:
		return "", "", fail("use a URL such as "+DefaultAddr, "bad --addr %q: the scheme must be http", raw)
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	path = u.Path
	if path == "" {
		path = "/gun"
	}
	return net.JoinHostPort(u.Hostname(), port), path, nil
}

// publicAddr is the URL to connect to, with the port actually bound.
func publicAddr(raw string, bound *net.TCPAddr, path string) string {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, _ := url.Parse(raw)
	host := u.Hostname()
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost" // listening everywhere: localhost is one of them
	}
	return "http://" + net.JoinHostPort(host, fmt.Sprint(bound.Port)) + path
}

func explainListen(host string, err error) error {
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		return fail("stop the other server, or pick another port: --addr http://localhost:8080/gun",
			"cannot listen on %s: the port is already in use", host)
	case errors.Is(err, syscall.EACCES):
		return fail("ports below 1024 need root; pick a higher one, like 8765", "cannot listen on %s: permission denied", host)
	}
	return fmt.Errorf("cannot listen on %s: %w", host, err)
}

// peerList reads --peers: comma-separated, and the flag repeated.
func peerList(values []string) ([]string, error) {
	var peers []string
	for _, v := range values {
		for p := range strings.SplitSeq(v, ",") {
			if p = strings.TrimSpace(p); p == "" {
				continue
			}
			if err := checkPeerURL("--peers", p); err != nil {
				return nil, err
			}
			peers = append(peers, p)
		}
	}
	return peers, nil
}

// openStore opens the store --store and --data ask for.
func openStore(c *ucli.Command, log *slog.Logger) (gundb.Store, func(), error) {
	kind := strings.ToLower(c.String("store"))
	if c.IsSet("data") && !c.IsSet("store") {
		kind = "pebble"
	}
	switch kind {
	case "memory", "mem":
		if c.IsSet("data") {
			return nil, nil, fail("drop --data, or use --store pebble", "--data is for --store pebble; memory keeps nothing on disk")
		}
		log.Info("storing data in memory: it is gone when the server stops (--store pebble keeps it)")
		return gundb.NewMemoryStore(), func() {}, nil
	case "pebble":
		dir := c.String("data")
		s, err := pebblestore.Open(dir, pebblestore.Options{NoSync: c.Bool("no-sync")})
		if err != nil {
			return nil, nil, withHint(fmt.Errorf("cannot open the data directory %s: %w", dir, err),
				"is another gundb server using it? each server needs its own --data")
		}
		abs, _ := filepath.Abs(dir)
		log.Info("storing data on disk with pebble", "data", abs)
		return s, func() { s.Close() }, nil
	}
	return nil, nil, fail("use --store memory or --store pebble", "unknown --store %q", c.String("store"))
}

// peerDialer dials --peers, logging a peer that cannot be reached once
// per outage rather than every 2s.
func peerDialer(log *slog.Logger) *dialer {
	var mu sync.Mutex
	down := map[string]bool{}
	d := &dialer{header: http.Header{}}
	d.onFail = func(addr string, err error) {
		mu.Lock()
		defer mu.Unlock()
		if !down[addr] {
			log.Warn("cannot reach peer, retrying every 2s", "peer", addr, "reason", err.Error())
			down[addr] = true
		}
	}
	d.onOK = func(addr string) {
		mu.Lock()
		defer mu.Unlock()
		delete(down, addr)
	}
	return d
}

func logPeer(log *slog.Logger, p gundb.PeerInfo, up, stopping bool) {
	switch {
	case p.URL != "" && up:
		log.Info("connected to peer", "peer", p.URL)
	case p.URL != "" && !stopping:
		log.Warn("lost peer, reconnecting", "peer", p.URL)
	case p.URL != "":
	case up:
		log.Info("client connected", "remote", p.Remote, "id", p.PeerID[:8])
	default:
		log.Info("client disconnected", "remote", p.Remote, "id", p.PeerID[:8])
	}
}
