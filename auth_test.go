package gundb

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ella.to/gundb/transport"
)

// authRelay is a relay where users/<name>/... is writable by <name> only,
// secrets/<name> is readable by <name> only, and tokens are "tok-<name>".
func authRelay(t *testing.T) (*DB, string) {
	t.Helper()
	relay := New(Options{
		Wait: 300 * time.Millisecond,
		Authenticate: func(r *http.Request) (string, error) {
			user, ok := strings.CutPrefix(r.URL.Query().Get("token"), "tok-")
			if !ok {
				return "", errors.New("bad token")
			}
			return user, nil
		},
		CanWrite: func(user, soul, field string) bool {
			path := strings.Split(soul+"/"+field, "/")
			if path[0] == "users" || path[0] == "secrets" {
				return path[1] == user
			}
			return true
		},
		CanRead: func(user, soul string) bool {
			path := strings.Split(soul, "/")
			return path[0] != "secrets" || len(path) < 2 || path[1] == user
		},
	})
	t.Cleanup(func() { relay.Close() })
	srv := httptest.NewServer(relay)
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

func TestAuthenticateRefusesBadTokens(t *testing.T) {
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
	if err := relay.write(ctx, g, false); err != nil {
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
	go relay.AttachAs(conn, "mallory")
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
