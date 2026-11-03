package gundb

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ella.to/gundb/transport"
)

// session is what the tests' HTTP middleware stores in the request
// context, the way an app stores its own session type.
type session struct{ user, device string }

type sessionKey struct{}

func withSession(ctx context.Context, s *session) context.Context {
	return context.WithValue(ctx, sessionKey{}, s)
}

// userOf returns the user of the connection behind ctx, "" if anonymous.
func userOf(ctx context.Context) string {
	if s, _ := ctx.Value(sessionKey{}).(*session); s != nil {
		return s.user
	}
	return ""
}

// requireToken is HTTP middleware: tokens are "tok-<name>", anything else
// is refused with 401, and the session goes into the request's context.
func requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := strings.CutPrefix(r.URL.Query().Get("token"), "tok-")
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s := &session{user: user, device: r.URL.Query().Get("device")}
		next.ServeHTTP(w, r.WithContext(withSession(r.Context(), s)))
	})
}

// authRules are the rules of authRelay:
//   - users/<name>/... and secrets/<name>/... are writable by <name> only;
//   - secrets/<name> is readable by <name> only;
//   - members is a private list: each user sees, and writes, only their
//     own entry, members.<name>, and the node it links to, members/<name>.
func authRules() Options {
	return Options{
		CanWrite: func(ctx context.Context, soul, field string) bool {
			path := strings.Split(soul+"/"+field, "/")
			switch path[0] {
			case "users", "secrets", "members":
				return path[1] == userOf(ctx)
			}
			return true
		},
		CanRead: func(ctx context.Context, soul, field string) bool {
			user := userOf(ctx)
			if soul == "members" {
				return field == user
			}
			path := strings.Split(soul, "/")
			switch path[0] {
			case "secrets", "members":
				return len(path) < 2 || path[1] == user
			}
			return true
		},
	}
}

// authRelay serves a DB with authRules behind requireToken.
func authRelay(t *testing.T) (*DB, string) {
	t.Helper()
	o := authRules()
	o.Wait = 300 * time.Millisecond
	relay := New(o)
	t.Cleanup(func() { relay.Close() })
	srv := httptest.NewServer(requireToken(relay))
	t.Cleanup(srv.Close)
	return relay, srv.URL + "/gun"
}

// client connects to the relay with a token and waits until it is in.
func client(t *testing.T, relay *DB, url, token string) *DB {
	t.Helper()
	before := len(relay.Peers())
	db := New(Options{Wait: 300 * time.Millisecond, Peers: []string{url + "?token=" + token}})
	t.Cleanup(func() { db.Close() })
	eventually(t, func() bool { return len(relay.Peers()) > before && len(db.Peers()) > 0 })
	return db
}

func TestMiddlewareRefusesBadTokens(t *testing.T) {
	relay, url := authRelay(t)
	db := New(Options{Peers: []string{url + "?token=nope"}})
	defer db.Close()
	time.Sleep(200 * time.Millisecond)
	if n := len(relay.Peers()); n != 0 {
		t.Fatalf("relay accepted %d peers with a bad token", n)
	}
	if err := db.Get("x").Get("y").PutAck(t.Context(), 1); !errors.Is(err, ErrNoPeers) {
		t.Fatalf("PutAck without a connection = %v", err)
	}
}

func TestCanWrite(t *testing.T) {
	ctx := t.Context()
	relay, url := authRelay(t)
	ali, bob := client(t, relay, url, "tok-ali"), client(t, relay, url, "tok-bob")

	msg := ali.Get("users").Get("ali").Get("messages").Get("m1")
	if err := msg.PutAck(ctx, map[string]string{"text": "hi"}); err != nil {
		t.Fatalf("ali writing her own messages: %v", err)
	}
	err := bob.Get("users").Get("ali").Get("messages").Get("m1").Get("text").PutAck(ctx, "hacked")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("bob writing ali's messages: got %v, want ErrForbidden", err)
	}
	if n, _ := relay.store.Get(ctx, "users/ali/messages/m1"); n == nil || n.Fields["text"] != String("hi") {
		t.Fatalf("relay holds %+v, want ali's text", n)
	}

	// Anyone may read it. (bob's own copy still has his rejected write:
	// writes apply locally before the relay answers.)
	carol := client(t, relay, url, "tok-carol")
	text, err := carol.Get("users").Get("ali").Get("messages").Get("m1").Get("text").Once[string](ctx)
	if err != nil || text != "hi" {
		t.Fatalf("carol reading ali's message: %q, %v", text, err)
	}
}

func TestCanRead(t *testing.T) {
	ctx := t.Context()
	relay, url := authRelay(t)
	ali, bob := client(t, relay, url, "tok-ali"), client(t, relay, url, "tok-bob")
	if err := ali.Get("secrets").Get("ali").Get("pin").PutAck(ctx, "1234"); err != nil {
		t.Fatal(err)
	}
	if pin, err := bob.Get("secrets").Get("ali").Get("pin").Once[string](ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob read ali's pin: %q, %v", pin, err)
	}
	fresh := client(t, relay, url, "tok-ali") // a new client, so the pin is not cached
	if pin, err := fresh.Get("secrets").Get("ali").Get("pin").Once[string](ctx); err != nil || pin != "1234" {
		t.Fatalf("ali reading her pin: %q, %v", pin, err)
	}
}

// Live updates and the relay's own writes are filtered per peer too.
func TestCanReadFiltersPushes(t *testing.T) {
	ctx := t.Context()
	relay, url := authRelay(t)
	bob := client(t, relay, url, "tok-bob")
	got := make(chan string, 10)
	off := bob.Get("public").Get("v").On(func(s string) { got <- s })
	defer off()

	// One local write on the relay touching a public and a secret node.
	g := graph{}
	g.node("public").Set("v", String("news"), 1)
	g.node("secrets/ali").Set("pin", String("1234"), 1)
	if err := relay.write(ctx, g, false, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-got:
		if v != "news" {
			t.Fatalf("bob got %q", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bob never got the public update")
	}
	if n, _ := bob.store.Get(ctx, "secrets/ali"); n != nil {
		t.Fatalf("bob received a node he may not read: %+v", n)
	}
}

// A peer cannot slip data in by answering a get the relay forwarded to it.
func TestForgedRepliesAreDropped(t *testing.T) {
	relay, _ := authRelay(t)
	conn := newScriptConn()
	go relay.Attach(withSession(t.Context(), &session{user: "mallory"}), conn)
	eventually(t, func() bool { return len(relay.Peers()) == 1 })

	go relay.Get("users").Get("ali").Get("name").Once[string](context.Background())
	var getID string
	for getID == "" {
		var m struct {
			ID  string          `json:"#"`
			Get json.RawMessage `json:"get"`
		}
		if json.Unmarshal(<-conn.sent, &m) == nil && m.Get != nil {
			getID = m.ID
		}
	}
	conn.recv <- []byte(`{"#":"forged","@":"` + getID + `","put":{"users":{"_":{"#":"users",">":{"ali":1}},"ali":{"#":"users/ali"}},"users/ali":{"_":{"#":"users/ali",">":{"name":1}},"name":"Mallory"}}}`)
	time.Sleep(200 * time.Millisecond)
	if n, _ := relay.store.Get(context.Background(), "users/ali"); n != nil {
		t.Fatalf("forged reply was stored: %+v", n)
	}
}

// scriptConn is a transport.Conn driven by the test.
type scriptConn struct {
	sent, recv chan []byte
	done       chan struct{}
	once       sync.Once
}

func newScriptConn() *scriptConn {
	return &scriptConn{sent: make(chan []byte, 100), recv: make(chan []byte), done: make(chan struct{})}
}

func (c *scriptConn) Send(_ context.Context, frame []byte) error {
	select {
	case c.sent <- append([]byte(nil), frame...):
	default:
	}
	return nil
}

func (c *scriptConn) Recv(ctx context.Context) ([]byte, error) {
	select {
	case f := <-c.recv:
		return f, nil
	case <-c.done:
	case <-ctx.Done():
	}
	return nil, transport.ErrClosed
}

func (c *scriptConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

func (c *scriptConn) RemoteAddr() string { return "script" }

// TestCanReadPerUserInFanout checks that a put going to many connections
// is filtered for each of them: connections that see the same fields share
// a view, and another view is not mixed in.
func TestCanReadPerUserInFanout(t *testing.T) {
	db := New(Options{CanRead: func(ctx context.Context, soul, _ string) bool { return soul != "secret" || userOf(ctx) == "admin" }})
	t.Cleanup(func() { db.Close() })
	connect := func(user string) *fakeConn {
		conn := newFakeConn(false)
		ctx, cancel := context.WithCancel(db.ctx)
		t.Cleanup(cancel)
		p := &peer{db: db, conn: conn, rules: withSession(ctx, &session{user: user}), ctx: ctx, cancel: cancel, outWake: make(chan struct{}, 1), wants: map[string]bool{}}
		go p.writeLoop()
		p.want("news")
		p.want("secret")
		return conn
	}
	admin, bob1, bob2 := connect("admin"), connect("bob"), connect("bob")
	writer := connect("writer")

	msgs, err := readFrame([]byte(`{"#":"m1","put":{` +
		`"news":{"_":{"#":"news",">":{"t":1}},"t":"hello"},` +
		`"secret":{"_":{"#":"secret",">":{"k":1}},"k":"s3cret"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(db.ctx)
	defer cancel()
	db.handle(&peer{db: db, conn: writer, rules: withSession(ctx, &session{user: "writer"}), ctx: ctx, cancel: cancel, outWake: make(chan struct{}, 1), wants: map[string]bool{}}, &msgs[0])

	souls := func(conn *fakeConn) []string {
		t.Helper()
		for {
			select {
			case frame := <-conn.frames:
				got, err := readFrame(frame)
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range got {
					if m.ID == "m1" {
						return slices.Sorted(maps.Keys(m.Put))
					}
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no put arrived")
			}
		}
	}
	if got := souls(admin); !slices.Equal(got, []string{"news", "secret"}) {
		t.Fatalf("admin got %v", got)
	}
	for i, bob := range []*fakeConn{bob1, bob2} {
		if got := souls(bob); !slices.Equal(got, []string{"news"}) {
			t.Fatalf("bob's connection %d got %v", i+1, got)
		}
	}
}

// A private list: each user reads their own entry of members, by path or
// by soul, while listing members, or its souls, shows nobody else.
func TestCanReadFieldsKeepAListPrivate(t *testing.T) {
	ctx := t.Context()
	relay, url := authRelay(t)
	ali, bob := client(t, relay, url, "tok-ali"), client(t, relay, url, "tok-bob")
	if err := ali.Get("members").Get("ali").Get("name").PutAck(ctx, "Ali"); err != nil {
		t.Fatal(err)
	}
	if err := bob.Get("members").Get("bob").Get("name").PutAck(ctx, "Bob"); err != nil {
		t.Fatal(err)
	}
	if err := bob.Get("members").Get("ali").Get("name").PutAck(ctx, "Mallory"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("bob writing ali's entry: %v", err)
	}

	fresh := client(t, relay, url, "tok-ali") // nothing cached
	if name, err := fresh.Get("members").Get("ali").Get("name").Once[string](ctx); err != nil || name != "Ali" {
		t.Fatalf("ali reading her own entry: %q, %v", name, err)
	}
	if name, err := fresh.Get("members/ali").Get("name").Once[string](ctx); err != nil || name != "Ali" {
		t.Fatalf("ali reading her node by soul: %q, %v", name, err)
	}
	if name, err := fresh.Get("members").Get("bob").Get("name").Once[string](ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ali reading bob's entry: %q, %v", name, err)
	}
	if name, err := fresh.Get("members/bob").Get("name").Once[string](ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ali reading bob's node by soul: %q, %v", name, err)
	}
	list, err := fresh.Get("members").Once[map[string]any](ctx)
	if err != nil || !slices.Equal(slices.Sorted(maps.Keys(list)), []string{"ali"}) {
		t.Fatalf("ali listing members: %v, %v", list, err)
	}
	souls, err := fresh.Souls(ctx, Lex{Prefix: "members"})
	if err != nil || !slices.Equal(souls, []string{"members", "members/ali"}) {
		t.Fatalf("ali listing souls: %q, %v", souls, err)
	}
}

// Live updates to a private list carry only the fields each reader may see.
func TestCanReadFieldsFilterPushes(t *testing.T) {
	ctx := t.Context()
	relay, url := authRelay(t)
	ali, bob := client(t, relay, url, "tok-ali"), client(t, relay, url, "tok-bob")
	got := make(chan []string, 10)
	off := ali.Get("members").On(func(list map[string]any) { got <- slices.Sorted(maps.Keys(list)) })
	defer off()
	if err := ali.Get("members").Get("ali").Get("name").PutAck(ctx, "Ali"); err != nil {
		t.Fatal(err)
	}
	if err := bob.Get("members").Get("bob").Get("name").PutAck(ctx, "Bob"); err != nil {
		t.Fatal(err)
	}
	// bob's entry is the newer write; once ali has seen hers, give his a
	// moment to (not) arrive.
	eventually(t, func() bool {
		select {
		case keys := <-got:
			return slices.Equal(keys, []string{"ali"})
		default:
			return false
		}
	})
	time.Sleep(200 * time.Millisecond)
	if n, _ := ali.store.Get(ctx, "members"); n == nil || n.Fields["bob"] != nil {
		t.Fatalf("ali holds %+v", n)
	}
	if n, _ := ali.store.Get(ctx, "members/bob"); n != nil {
		t.Fatalf("ali received bob's node: %+v", n)
	}
}

// The rules see the connection's own context, and cancelling that context
// drops the connection: a way to log a user out of a live connection.
func TestRulesSeeTheConnectionContext(t *testing.T) {
	var mu sync.Mutex
	var wrote []string
	relay := New(Options{CanWrite: func(ctx context.Context, soul, field string) bool {
		mu.Lock()
		defer mu.Unlock()
		wrote = append(wrote, userOf(ctx)+":"+soul+"."+field)
		return true
	}})
	t.Cleanup(func() { relay.Close() })
	conn := newScriptConn()
	ctx, logout := context.WithCancel(withSession(t.Context(), &session{user: "ali"}))
	done := make(chan struct{})
	go func() { relay.Attach(ctx, conn); close(done) }()
	eventually(t, func() bool { return len(relay.Peers()) == 1 })

	conn.recv <- []byte(`{"#":"w1","put":{"n":{"_":{"#":"n",">":{"v":1}},"v":1}}}`)
	eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(wrote) == 1 })
	if wrote[0] != "ali:n.v" {
		t.Fatalf("CanWrite saw %q", wrote)
	}
	logout()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelling the context did not drop the connection")
	}
}
