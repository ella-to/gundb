package gundb

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

type Member struct {
	Name string  `json:"name"`
	Age  int     `json:"age,omitempty"`
	Boss *Member `json:"boss,omitempty"`
}

func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("condition not met in time")
}

// link makes b dial a over a real WebSocket and waits until both are connected.
func link(t *testing.T, a, b *DB) {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	na, nb := len(a.Peers()), len(b.Peers())
	b.Connect(srv.URL + "/gun")
	eventually(t, func() bool { return len(a.Peers()) > na && len(b.Peers()) > nb })
}

func newDB(t *testing.T) *DB {
	db := New(Options{Wait: 500 * time.Millisecond})
	t.Cleanup(func() { db.Close() })
	return db
}

// collector gathers callback values for assertions.
type collector[T any] struct {
	mu  sync.Mutex
	got []T
}

func (c *collector[T]) add(v T) { c.mu.Lock(); c.got = append(c.got, v); c.mu.Unlock() }
func (c *collector[T]) last() (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero T
	if len(c.got) == 0 {
		return zero, false
	}
	return c.got[len(c.got)-1], true
}

func TestPutOnceLocal(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)

	if err := db.Get("alice").Put(ctx, Member{Name: "Alice", Age: 30}); err != nil {
		t.Fatal(err)
	}
	u, err := db.Get("alice").Once[Member](ctx)
	if err != nil || u.Name != "Alice" || u.Age != 30 {
		t.Fatalf("%+v %v", u, err)
	}
	name, err := db.Get("alice").Get("name").Once[string](ctx)
	if err != nil || name != "Alice" {
		t.Fatalf("%q %v", name, err)
	}
	if _, err := db.Get("nobody").Once[Member](ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// Merges are locked per soul: concurrent writes to one node must not lose
// fields, and writes to different nodes must all land.
func TestConcurrentLocalMerges(t *testing.T) {
	ctx := t.Context()
	db := New()
	defer db.Close()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			if err := db.Get("shared").Get(fmt.Sprint("f", i)).Put(ctx, i); err != nil {
				t.Error(err)
			}
			if err := db.Get(fmt.Sprint("own", i)).Get("v").Put(ctx, i); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	shared, err := db.Get("shared").Once[map[string]int](ctx)
	if err != nil || len(shared) != 50 {
		t.Fatalf("shared node has %d fields, err %v", len(shared), err)
	}
	for i := range 50 {
		if v, err := db.Get(fmt.Sprint("own", i)).Get("v").Once[int](ctx); err != nil || v != i {
			t.Fatalf("own%d = %d, %v", i, v, err)
		}
	}
}

func TestPutMergesFields(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	db.Get("alice").Put(ctx, map[string]any{"name": "Alice"})
	db.Get("alice").Put(ctx, map[string]any{"age": 31})
	db.Get("alice").Get("city").Put(ctx, "Paris")
	got, _ := db.Get("alice").Once[map[string]any](ctx)
	if got["name"] != "Alice" || got["age"] != 31.0 || got["city"] != "Paris" {
		t.Fatalf("%v", got)
	}
}

func TestNestedStructsBecomeLinkedNodes(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	db.Get("mark").Put(ctx, Member{Name: "Mark", Boss: &Member{Name: "Fluffy"}})

	// Same souls GUN JS would create.
	n, _ := db.store.Get(ctx, "mark")
	if n.Fields["boss"] != (Link{Soul: "mark/boss"}) {
		t.Fatalf("boss = %#v", n.Fields["boss"])
	}
	u, _ := db.Get("mark").Once[Member](ctx)
	if u.Boss == nil || u.Boss.Name != "Fluffy" {
		t.Fatalf("%+v", u)
	}

	// Writing through the path updates the same linked node.
	db.Get("mark").Get("boss").Get("name").Put(ctx, "Fluffy II")
	boss, _ := db.Get("mark").Get("boss").Once[Member](ctx)
	if boss.Name != "Fluffy II" {
		t.Fatalf("%+v", boss)
	}
}

func TestPutRefStoresLink(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	db.Get("bob").Put(ctx, Member{Name: "Bob"})
	db.Get("alice").Put(ctx, Member{Name: "Alice"})
	db.Get("alice").Get("boss").Put(ctx, db.Get("bob"))

	u, _ := db.Get("alice").Once[Member](ctx)
	if u.Boss == nil || u.Boss.Name != "Bob" {
		t.Fatalf("%+v", u)
	}
}

func TestCyclesAreSafe(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	db.Get("a").Put(ctx, map[string]any{"name": "a"})
	db.Get("b").Put(ctx, map[string]any{"name": "b", "friend": db.Get("a")})
	db.Get("a").Get("friend").Put(ctx, db.Get("b"))
	u, err := db.Get("a").Once[map[string]any](ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u["friend"].(map[string]any)["friend"].(map[string]any)["#"] != "a" {
		t.Fatalf("%v", u)
	}
}

func TestDelete(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	db.Get("alice").Put(ctx, Member{Name: "Alice", Age: 3})
	db.Get("alice").Get("age").Put(ctx, nil)
	if _, err := db.Get("alice").Get("age").Once[int](ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPutErrors(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	if err := db.Get("x").Put(ctx, "just a string"); err == nil {
		t.Fatal("primitive on a root should fail")
	}
	if err := db.Get("x").Put(ctx, map[string]any{"list": []int{1, 2}}); err == nil {
		t.Fatal("arrays should fail")
	}
}

func TestOn(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	var c collector[Member]
	off := db.Get("alice").On(c.add)
	defer off()

	db.Get("alice").Put(ctx, Member{Name: "Alice"})
	eventually(t, func() bool { u, _ := c.last(); return u.Name == "Alice" })

	// Changes in a linked node fire too.
	db.Get("alice").Get("boss").Put(ctx, Member{Name: "Bob"})
	eventually(t, func() bool { u, _ := c.last(); return u.Boss != nil && u.Boss.Name == "Bob" })
	db.Get("alice").Get("boss").Get("name").Put(ctx, "Robert")
	eventually(t, func() bool { u, _ := c.last(); return u.Boss != nil && u.Boss.Name == "Robert" })

	off()
	n := len(c.got)
	db.Get("alice").Put(ctx, Member{Name: "Changed"})
	time.Sleep(50 * time.Millisecond)
	if len(c.got) != n {
		t.Fatal("callback fired after off")
	}
}

// TestOnStartsWithValueAtSubscribe checks that the first callback of On and
// Map().On is the value as it was when they were called, and that a change
// made right after still fires its own callback. The initial read used to
// run on the subscription's goroutine, racing with that change: sometimes
// the two were merged into a single callback.
func TestOnStartsWithValueAtSubscribe(t *testing.T) {
	ctx := t.Context()
	for i := range 200 {
		db := newDB(t)
		db.Get("user1").Put(ctx, Member{Name: "John"})
		db.Get("device1").Put(ctx, map[string]any{"name": "Device 1"})

		var c collector[map[string]any]
		off := db.Get("device1").On(c.add)
		var items collector[string]
		offMap := db.Get("device1").Get("users").Map().On(func(key string, _ Member) { items.add(key) })
		db.Get("device1").Get("users").Set(ctx, db.Get("user1"))

		eventually(t, func() bool { u, _ := c.last(); return u["users"] != nil })
		eventually(t, func() bool { _, ok := items.last(); return ok })
		off()
		offMap()

		c.mu.Lock()
		if len(c.got) != 2 || c.got[0]["users"] != nil {
			t.Fatalf("run %d: want the value before Set, then after; got %v", i, c.got)
		}
		c.mu.Unlock()
		db.Close()
	}
}

type Todo struct {
	Title string `json:"title"`
	Done  bool   `json:"done"`
}

func TestSetAndMap(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	todos := db.Get("todos")

	var mu sync.Mutex
	seen := map[string]*Todo{}
	off := todos.Map().On(func(id string, t *Todo) {
		mu.Lock()
		seen[id] = t
		mu.Unlock()
	})
	defer off()

	milk, err := todos.Set(ctx, Todo{Title: "milk"})
	if err != nil {
		t.Fatal(err)
	}
	todos.Set(ctx, Todo{Title: "eggs"})
	count := func(f func(*Todo) bool) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, t := range seen {
			if f(t) {
				n++
			}
		}
		return n
	}
	eventually(t, func() bool { return count(func(t *Todo) bool { return t != nil }) == 2 })

	milk.Get("done").Put(ctx, true)
	eventually(t, func() bool { return count(func(t *Todo) bool { return t != nil && t.Done }) == 1 })

	todos.Get(milk.Key()).Put(ctx, nil) // remove from the set
	eventually(t, func() bool { return count(func(t *Todo) bool { return t == nil }) == 1 })

	all, err := todos.Once[map[string]Todo](ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("%v %v", all, err)
	}
	for _, v := range all {
		if v.Title != "eggs" {
			t.Fatalf("%v", all)
		}
	}
}

func TestNodeAndLinks(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	todos := db.Get("todos")
	milk, err := todos.Set(ctx, Todo{Title: "milk"})
	if err != nil {
		t.Fatal(err)
	}
	eggs, err := todos.Set(ctx, Todo{Title: "eggs"})
	if err != nil {
		t.Fatal(err)
	}
	todos.Get("owner").Put(ctx, "alice")
	todos.Get(milk.Key()).Put(ctx, nil)

	n, err := todos.Node(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n.Soul != "todos" || n.Fields["owner"] != String("alice") ||
		n.Fields[milk.Key()] != (Null{}) || n.Fields[eggs.Key()] != (Link{Soul: eggs.Key()}) {
		t.Fatalf("%+v", n)
	}
	if _, ok := n.States["owner"]; !ok {
		t.Fatalf("no state for owner: %+v", n.States)
	}

	links, err := todos.Links(ctx)
	if err != nil || !maps.Equal(links, map[string]string{eggs.Key(): eggs.Key()}) {
		t.Fatalf("%v %v", links, err)
	}

	// A path through a link reads the linked node.
	db.Get("alice").Put(ctx, Member{Name: "Alice", Boss: &Member{Name: "Bob"}})
	boss, err := db.Get("alice").Get("boss").Node(ctx)
	if err != nil || boss.Soul != "alice/boss" || boss.Fields["name"] != String("Bob") {
		t.Fatalf("%+v %v", boss, err)
	}
	if links, err := db.Get("alice").Links(ctx); err != nil || !maps.Equal(links, map[string]string{"boss": "alice/boss"}) {
		t.Fatalf("%v %v", links, err)
	}

	if _, err := db.Get("nobody").Node(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := todos.Get(milk.Key()).Node(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted item: want ErrNotFound, got %v", err)
	}
	if _, err := todos.Get("owner").Links(ctx); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("value: want a not-a-node error, got %v", err)
	}
}

// Links on a peer's set fetches the set node only, not its items.
func TestLinksDoesNotLoadItems(t *testing.T) {
	ctx := t.Context()
	relay, client := newDB(t), newDB(t)
	var souls []string
	for i := range 100 {
		item, err := relay.Get("list").Set(ctx, Todo{Title: fmt.Sprint("t", i)})
		if err != nil {
			t.Fatal(err)
		}
		souls = append(souls, item.Key())
	}
	link(t, relay, client)

	links, err := client.Get("list").Links(ctx)
	if err != nil || len(links) != 100 {
		t.Fatalf("%d links, %v", len(links), err)
	}
	for _, soul := range souls {
		if links[soul] != soul {
			t.Fatalf("missing %s", soul)
		}
		if n, _ := client.store.Get(ctx, soul); n != nil {
			t.Fatalf("item %s was loaded", soul)
		}
	}

	// The items are still there to read one by one.
	todo, err := client.Get(souls[7]).Once[Todo](ctx)
	if err != nil || todo.Title != "t7" {
		t.Fatalf("%+v %v", todo, err)
	}
}

func TestSyncBetweenPeers(t *testing.T) {
	ctx := t.Context()
	a, b := newDB(t), newDB(t)
	link(t, a, b)

	var c collector[Member]
	defer b.Get("alice").On(c.add)()

	a.Get("alice").Put(ctx, Member{Name: "Alice", Boss: &Member{Name: "Bob"}})
	eventually(t, func() bool { u, _ := c.last(); return u.Boss != nil && u.Boss.Name == "Bob" })

	// Once on a peer that has never seen the data asks the network.
	d := newDB(t)
	link(t, a, d)
	u, err := d.Get("alice").Once[Member](ctx)
	if err != nil || u.Boss == nil || u.Boss.Name != "Bob" {
		t.Fatalf("%+v %v", u, err)
	}
}

func TestRelay(t *testing.T) {
	// alice <-> relay <-> bob: bob only hears about souls it asked for.
	ctx := t.Context()
	relay, alice, bob := newDB(t), newDB(t), newDB(t)
	link(t, relay, alice)
	link(t, relay, bob)

	var c collector[string]
	defer bob.Get("room").Get("topic").On(c.add)()
	time.Sleep(50 * time.Millisecond) // let bob's subscription reach the relay

	alice.Get("room").Get("topic").Put(ctx, "hello")
	eventually(t, func() bool { v, _ := c.last(); return v == "hello" })

	alice.Get("other").Put(ctx, map[string]any{"x": 1})
	time.Sleep(50 * time.Millisecond)
	if n, _ := bob.store.Get(ctx, "other"); n != nil {
		t.Fatal("relay forwarded a soul bob never asked for")
	}

	// GETs for data only alice has travel through the relay and back.
	alice.Get("secret").Put(ctx, map[string]any{"v": "42"})
	v, err := bob.Get("secret").Get("v").Once[string](ctx)
	if err != nil || v != "42" {
		t.Fatalf("%q %v", v, err)
	}
}

func TestPutAck(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	a, b := newDB(t), newDB(t)
	if err := a.Get("x").PutAck(ctx, map[string]any{"v": 1}); !errors.Is(err, ErrNoPeers) {
		t.Fatalf("want ErrNoPeers, got %v", err)
	}
	link(t, a, b)
	if err := a.Get("x").PutAck(ctx, map[string]any{"v": 2}); err != nil {
		t.Fatal(err)
	}
	if v, _ := b.Get("x").Get("v").Once[int](ctx); v != 2 {
		t.Fatalf("got %d", v)
	}
}

func TestOfflineWritesSyncOnConnect(t *testing.T) {
	ctx := t.Context()
	old := redialEvery
	redialEvery = 20 * time.Millisecond
	defer func() { redialEvery = old }()

	// Reserve an address, but don't serve on it yet.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	// The client starts while the server is down; its write is queued and
	// delivered once the connection comes up.
	client := New(Options{Peers: []string{"ws://" + addr + "/gun"}})
	defer client.Close()
	client.Get("note").Put(ctx, map[string]any{"text": "written early"})

	server := newDB(t)
	l, err = net.Listen("tcp", addr)
	if err != nil {
		t.Skip("address taken meanwhile:", err)
	}
	hs := &http.Server{Handler: server}
	go hs.Serve(l)
	defer hs.Close()

	eventually(t, func() bool {
		n, _ := server.store.Get(ctx, "note")
		return n != nil
	})
}

// A write made offline is only in the store once the DB that made it is
// gone (a page reload). The next DB on that store must still deliver it.
func TestOfflineWritesSurviveRestart(t *testing.T) {
	ctx := t.Context()
	store := NewMemoryStore()
	offline := New(Options{Store: store})
	if err := offline.Get("notes").Put(ctx, map[string]any{"b": "written offline"}); err != nil {
		t.Fatal(err)
	}
	offline.Close()

	relay, watcher := newDB(t), newDB(t)
	link(t, relay, watcher)
	var got collector[string]
	defer watcher.Get("notes").Get("b").On(got.add)()
	time.Sleep(100 * time.Millisecond) // the watcher's get finds nothing

	restarted := New(Options{Store: store, Wait: 500 * time.Millisecond})
	defer restarted.Close()
	link(t, relay, restarted)
	if _, err := restarted.Get("notes").Once[map[string]any](ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { v, _ := got.last(); return v == "written offline" })
}

func TestConcurrentWrites(t *testing.T) {
	ctx := t.Context()
	a, b := newDB(t), newDB(t)
	link(t, a, b)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() { a.Get("counter").Get("a").Put(ctx, i) })
		wg.Go(func() { b.Get("counter").Get("b").Put(ctx, i) })
	}
	wg.Wait()
	eventually(t, func() bool {
		x, _ := a.Get("counter").Once[map[string]int](ctx)
		y, _ := b.Get("counter").Once[map[string]int](ctx)
		return len(x) == 2 && x["a"] == y["a"] && x["b"] == y["b"]
	})
}

// A relay forwards puts only to the peers that asked for the soul, and the
// index of who asked is cleaned up when a peer leaves.
func TestRelayForwardsToSubscribersOnly(t *testing.T) {
	ctx := t.Context()
	relay, writer, other := newDB(t), newDB(t), newDB(t)
	fan := New(Options{Wait: 500 * time.Millisecond})
	defer fan.Close()
	for _, p := range []*DB{writer, fan, other} {
		link(t, relay, p)
	}
	if err := writer.Get("news").Get("v").PutAck(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if v, err := fan.Get("news").Get("v").Once[string](ctx); err != nil || v != "first" {
		t.Fatalf("fan read %q, %v", v, err)
	}
	relay.subsMu.Lock()
	n := len(relay.subs["news"])
	relay.subsMu.Unlock()
	if n != 1 {
		t.Fatalf("news has %d subscribers, want 1", n)
	}

	if err := writer.Get("news").Get("v").PutAck(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		n, _ := fan.store.Get(ctx, "news")
		return n != nil && n.Fields["v"] == String("second")
	})
	if n, _ := other.store.Get(ctx, "news"); n != nil {
		t.Fatalf("a peer that never asked got %+v", n)
	}

	fan.Close()
	eventually(t, func() bool {
		relay.subsMu.Lock()
		defer relay.subsMu.Unlock()
		return len(relay.subs) == 0
	})
}

func TestSelfConnectionIsDropped(t *testing.T) {
	a := New(Options{PID: "same"})
	defer a.Close()
	srv := httptest.NewServer(a)
	defer srv.Close()
	a.Connect(srv.URL + "/gun") // dial ourselves
	time.Sleep(100 * time.Millisecond)
	eventually(t, func() bool { return len(a.Peers()) == 0 })
}

func TestClose(t *testing.T) {
	a, b := New(), New()
	defer b.Close()
	link(t, a, b)
	a.Close()
	eventually(t, func() bool { return len(b.Peers()) == 0 })
	if err := a.Get("x").Put(t.Context(), map[string]any{"v": 1}); !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}

// TestGetOneField checks the answers to gets for one field of a node.
func TestGetOneField(t *testing.T) {
	db := newDB(t)
	if err := db.Get("n").Put(t.Context(), map[string]any{"a": 1, "b": 2, "c": 3}); err != nil {
		t.Fatal(err)
	}
	conn := newFakeConn(false)
	ctx, cancel := context.WithCancel(db.ctx)
	defer cancel()
	p := &peer{db: db, conn: conn, ctx: ctx, cancel: cancel, outWake: make(chan struct{}, 1), wants: map[string]bool{}}
	go p.writeLoop()
	ask := func(id, get string) message {
		t.Helper()
		msgs, err := readFrame([]byte(`{"#":"` + id + `","get":` + get + `}`))
		if err != nil {
			t.Fatal(err)
		}
		db.handle(p, &msgs[0])
		select {
		case frame := <-conn.frames:
			reply, err := readFrame(frame)
			if err != nil || len(reply) != 1 || reply[0].Ack != id {
				t.Fatalf("reply to %s: %s (%v)", id, frame, err)
			}
			return reply[0]
		case <-time.After(3 * time.Second):
			t.Fatalf("no reply to %s", id)
		}
		panic("unreachable")
	}
	fields := func(m message, soul string) []string {
		if m.Put[soul] == nil {
			return nil
		}
		return slices.Sorted(maps.Keys(m.Put[soul].Fields))
	}

	if got := fields(ask("g1", `{"#":"n",".":"b"}`), "n"); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("one field: got %v", got)
	}
	if m := ask("g2", `{"#":"n",".":"missing"}`); m.Put != nil {
		t.Fatalf("missing field: got %v", m.Put)
	}
	if m := ask("g3", `{"#":"nobody",".":"a"}`); m.Put != nil {
		t.Fatalf("missing soul: got %v", m.Put)
	}
	if got := fields(ask("g4", `{"#":"n",".":{">":"b"}}`), "n"); !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("lex on fields: got %v", got)
	}
}
