package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	ucli "github.com/urfave/cli/v3"

	"ella.to/gundb"
	"ella.to/gundb/transport"
	"ella.to/gundb/transport/ws"
)

// dialer is a WebSocket dialer that sends the --header headers and keeps
// the last failure, explained, so a command can say why it is not
// connected.
type dialer struct {
	header http.Header
	onFail func(addr string, err error) // optional, after each failed dial
	onOK   func(addr string)            // optional, after each successful dial

	mu   sync.Mutex
	last error
}

func (d *dialer) Dial(ctx context.Context, addr string) (transport.Conn, error) {
	c, resp, err := websocket.Dial(ctx, wsURL(addr), &websocket.DialOptions{HTTPHeader: d.header})
	if err != nil {
		if ctx.Err() != nil {
			return nil, err // given up on, not refused: nothing to report
		}
		err = explainDial(addr, resp, err)
		d.mu.Lock()
		d.last = err
		d.mu.Unlock()
		if d.onFail != nil {
			d.onFail(addr, err)
		}
		return nil, err
	}
	c.SetReadLimit(64 << 20)
	d.mu.Lock()
	d.last = nil
	d.mu.Unlock()
	if d.onOK != nil {
		d.onOK(addr)
	}
	return ws.NewConn(c, addr), nil
}

func (d *dialer) lastErr() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.last
}

func wsURL(addr string) string {
	if rest, ok := strings.CutPrefix(addr, "http://"); ok {
		return "ws://" + rest
	}
	if rest, ok := strings.CutPrefix(addr, "https://"); ok {
		return "wss://" + rest
	}
	return addr
}

// explainDial says why dialling addr failed, in words.
func explainDial(addr string, resp *http.Response, err error) error {
	if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
		body := ""
		if resp.Body != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
			body = strings.TrimSpace(string(b))
		}
		status := resp.Status
		if body != "" {
			status += ": " + body
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fail("send credentials with --header, e.g. --header \"Authorization=Bearer <token>\"",
				"%s refused the connection (%s)", addr, status)
		case http.StatusNotFound:
			return fail("check the path in --addr; gundb serves on /gun by default", "%s has no GUN endpoint (%s)", addr, status)
		}
		return fmt.Errorf("%s refused the connection (%s)", addr, status)
	}
	var dns *net.DNSError
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return fail("start one with: gundb server, or point --addr at a running one", "nothing is listening at %s", addr)
	case errors.As(err, &dns):
		return fail("check the host name in --addr", "unknown host %q", dns.Name)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return err
	}
	return fmt.Errorf("cannot connect to %s: %w", addr, err)
}

// parseHeaders reads --header values: "Name=Value" or "Name: Value".
func parseHeaders(values []string) (http.Header, error) {
	h := http.Header{}
	for _, v := range values {
		name, value, ok := strings.Cut(v, "=")
		if colon := strings.Index(v, ":"); colon >= 0 && (!ok || colon < len(name)) {
			name, value, ok = v[:colon], v[colon+1:], true
		}
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || name == "" {
			return nil, fail(`write it as --header "Name=Value", e.g. --header "Authorization=Bearer 1234"`, "bad --header %q", v)
		}
		h.Add(name, value)
	}
	return h, nil
}

// checkPeerURL rejects a URL gundb cannot dial.
func checkPeerURL(flag, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || !slices.Contains([]string{"http", "https", "ws", "wss"}, u.Scheme) {
		return fail("use a full URL such as "+DefaultAddr, "bad %s %q", flag, raw)
	}
	return nil
}

// client is a connection to the server, for one command.
type client struct {
	db      *gundb.DB
	addr    string
	user    *gundb.User // logged in with --username, or nil
	timeout time.Duration
	closing atomic.Bool // close was called: disconnecting is expected
}

// connect connects to --addr, waiting at most until ctx is done, and with
// login, logs in if --username is set.
func (a *App) connect(ctx context.Context, c *ucli.Command, log *slog.Logger, login bool) (*client, error) {
	_, lib := a.logger(c)
	addr := c.String("addr")
	if err := checkPeerURL("--addr", addr); err != nil {
		return nil, err
	}
	header, err := parseHeaders(c.StringSlice("header"))
	if err != nil {
		return nil, err
	}
	d := &dialer{header: header}

	// After the first connection, tell the person when it drops and
	// comes back (sub runs for a long time).
	var mu sync.Mutex
	connected := false
	cl := &client{addr: addr, timeout: c.Duration("timeout")}
	db := gundb.New(gundb.Options{
		Peers:  []string{addr},
		Dialer: d,
		Logger: lib,
		OnConnect: func(context.Context, gundb.PeerInfo) {
			mu.Lock()
			defer mu.Unlock()
			if connected {
				log.Info("reconnected", "addr", addr)
			}
			connected = true
		},
		OnDisconnect: func(context.Context, gundb.PeerInfo) {
			if !cl.closing.Load() {
				log.Warn("lost the connection, reconnecting", "addr", addr)
			}
		},
	})
	cl.db = db

	warned := false
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for len(db.Peers()) == 0 {
		select {
		case <-ctx.Done():
			cl.close()
			if err := d.lastErr(); err != nil && cl.timeout == 0 {
				return nil, err // interrupted while waiting: say why it never connected
			}
			return nil, explain(ctx.Err(), "connect to "+addr, cl.timeout)
		case <-tick.C:
		}
		err := d.lastErr()
		switch {
		case err == nil:
		case cl.timeout > 0:
			// The server answered no; waiting for the next try in 2s
			// rarely helps. --timeout 0 waits for it.
			cl.close()
			return nil, err
		case !warned:
			log.Warn("waiting for the server, retrying every 2s (Ctrl-C to stop)", "reason", err.Error())
			warned = true
		}
	}
	log.Debug("connected", "addr", addr)

	if name := c.String("username"); login && name != "" {
		password := c.String("password")
		if password == "" {
			cl.close()
			return nil, fail("add --password, or set GUNDB_PASSWORD", "--username %s needs a --password", name)
		}
		u, err := db.Login(ctx, name, password)
		if err != nil {
			cl.close()
			return nil, explain(err, "log in as "+name, cl.timeout)
		}
		cl.user = u
		log.Debug("logged in", "user", name, "pub", u.Pub())
	}
	return cl, nil
}

func (cl *client) close() {
	cl.closing.Store(true)
	cl.db.Close()
}

// ref returns the ref a path names:
//
//	users/1/name   db.Get("users").Get("1").Get("name")
//	~/profile      the logged-in user's space, user.Get("profile")
//	~@ali/profile  ali's space, read-only
//	~<pub>/profile the space of the user with that public key
func (cl *client) ref(ctx context.Context, path string) (*gundb.Ref, error) {
	keys, err := splitPath(path)
	if err != nil {
		return nil, err
	}
	var ref *gundb.Ref
	switch first := keys[0]; {
	case first == "~":
		if cl.user == nil {
			return nil, fail("log in with --username and --password", "%s is in your own user space (~/), but you are not logged in", path)
		}
		ref = cl.user.Root()
	case strings.HasPrefix(first, "~@"):
		u, err := cl.lookUp(ctx, first)
		if err != nil {
			return nil, err
		}
		ref = cl.db.User(u.pub)
	default:
		ref = cl.db.Get(first)
	}
	for _, k := range keys[1:] {
		ref = ref.Get(k)
	}
	return ref, nil
}

// splitPath splits "users/1/name" into its keys.
func splitPath(path string) ([]string, error) {
	path = unexpandHome(path)
	trimmed := strings.TrimSuffix(path, "/")
	if trimmed == "" {
		return nil, fail("name a place in the graph, like users/1/name", "empty path")
	}
	keys := strings.Split(trimmed, "/")
	if slices.Contains(keys, "") {
		return nil, fail("write it like users/1/name, one key per segment", "path %q has an empty key", path)
	}
	return keys, nil
}

// userKeys are a user's public keys.
type userKeys struct {
	alias     string // as given, for messages
	pub, epub string
}

// lookUp finds the public keys of a user given as alias ("ali", "@ali",
// "~@ali") or public key ("<pub>", "~<pub>").
func (cl *client) lookUp(ctx context.Context, who string) (userKeys, error) {
	name := strings.TrimPrefix(who, "~")
	k := userKeys{alias: who}
	if cl.user != nil && (name == "@"+cl.user.Alias() || name == cl.user.Alias() || name == cl.user.Pub()) {
		p := cl.user.Pair()
		return userKeys{alias: who, pub: p.Pub, epub: p.EPub}, nil
	}
	if alias, isAlias := strings.CutPrefix(name, "@"); isAlias || !looksLikePub(name) {
		links, err := cl.db.Get("~@" + alias).Links(ctx)
		if err != nil && !errors.Is(err, gundb.ErrNotFound) {
			return k, explain(err, "look up user "+alias, cl.timeout)
		}
		souls := slices.Sorted(maps.Keys(links))
		if len(souls) == 0 {
			return k, fail("create it with: gundb user create -u "+alias+" -p <password>", "no user named %q", alias)
		}
		name = strings.TrimPrefix(souls[0], "~")
	}
	k.pub = name
	epub, err := cl.db.User(name).Get("epub").Once[string](ctx)
	if err != nil {
		return k, explain(err, "read the keys of "+who, cl.timeout)
	}
	k.epub = epub
	return k, nil
}

// looksLikePub reports whether s is a SEA public key: "x.y", base64url.
func looksLikePub(s string) bool {
	x, y, ok := strings.Cut(s, ".")
	return ok && len(x) >= 40 && len(y) >= 40
}

// unexpandHome undoes the shell's tilde expansion: an unquoted ~/notes
// arrives as /home/ali/notes. No GUN path starts with "/", so a path
// starting with the home directory can only have been ~/.
func unexpandHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !strings.HasPrefix(path, "/") {
		return path
	}
	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, home+"/"); ok {
		return "~/" + rest
	}
	return path
}
