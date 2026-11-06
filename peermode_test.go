package gundb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// serve makes db reachable over a real WebSocket and returns its URL.
func serve(t *testing.T, db *DB) string {
	t.Helper()
	srv := httptest.NewServer(db)
	t.Cleanup(srv.Close)
	return srv.URL + "/gun"
}

// dialer returns a DB that dials peer (a URL, maybe wrapped in ReadFrom
// or WriteTo) and waits until it is connected.
func dialer(t *testing.T, server *DB, peer string) *DB {
	t.Helper()
	before := len(server.Peers())
	db := New(Options{Wait: 300 * time.Millisecond, Peers: []string{peer}})
	t.Cleanup(func() { db.Close() })
	eventually(t, func() bool { return len(server.Peers()) > before && len(db.Peers()) > 0 })
	return db
}

// stored reports whether db's own store holds soul.
func stored(t *testing.T, db *DB, soul string) bool {
	t.Helper()
	n, err := db.store.Get(context.Background(), soul)
	return err == nil && n != nil
}

func TestReadFromPeer(t *testing.T) {
	ctx := t.Context()
	primary := newDB(t)
	replica := dialer(t, primary, ReadFrom(serve(t, primary)))

	// The replica reads what the primary has, and follows its changes.
	if err := primary.Get("prices").Put(ctx, map[string]int{"milk": 1}); err != nil {
		t.Fatal(err)
	}
	if v, err := replica.Get("prices").Get("milk").Once[int](ctx); err != nil || v != 1 {
		t.Fatalf("replica reading the primary: %v, %v", v, err)
	}
	var mu sync.Mutex
	var seen []int
	off := replica.Get("prices").Get("milk").On(func(v int) { mu.Lock(); seen = append(seen, v); mu.Unlock() })
	defer off()
	primary.Get("prices").Get("milk").Put(ctx, 2)
	eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(seen) > 0 && seen[len(seen)-1] == 2 })

	// What the replica writes stays on the replica.
	if err := replica.Get("drafts").Get("x").PutAck(ctx, "local"); !errors.Is(err, ErrNoPeers) {
		t.Fatalf("PutAck with only a peer we read from: %v, want ErrNoPeers", err)
	}
	if !stored(t, replica, "drafts") {
		t.Fatal("the replica lost its own write")
	}
	// Clients of the replica write to it; that doesn't reach the primary.
	client := dialer(t, replica, serve(t, replica))
	if err := client.Get("drafts").Get("y").PutAck(ctx, "from a client"); err != nil {
		t.Fatalf("a client writing to the replica: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if stored(t, primary, "drafts") {
		t.Fatal("the replica wrote to a peer it only reads from")
	}
	// And the primary asking for it gets nothing from the replica.
	if _, err := primary.Get("drafts").Get("x").Once[string](ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the primary reading the replica's data: %v, want ErrNotFound", err)
	}
	if stored(t, primary, "drafts") {
		t.Fatal("the replica answered a peer it only reads from")
	}
}

func TestWriteToPeer(t *testing.T) {
	ctx := t.Context()
	backup := newDB(t)
	edge := dialer(t, backup, WriteTo(serve(t, backup)))

	// What the edge writes reaches the backup, with an ack.
	if err := edge.Get("orders").Get("1").PutAck(ctx, map[string]int{"qty": 2}); err != nil {
		t.Fatalf("writing to the backup: %v", err)
	}
	if !stored(t, backup, "orders/1") {
		t.Fatal("the backup didn't get the write")
	}

	// Nothing comes back from the backup: not its own writes...
	backup.Get("injected").Get("x").Put(ctx, "from the backup")
	time.Sleep(200 * time.Millisecond)
	if stored(t, edge, "injected") {
		t.Fatal("the edge took data from a peer it only writes to")
	}
	// ...and the edge never asks it.
	start := time.Now()
	if _, err := edge.Get("injected").Get("x").Once[string](ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the edge reading the backup: %v, want ErrNotFound", err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Errorf("a read with no peer to ask waited %s", time.Since(start))
	}

	// The backup may still read from the edge.
	g := graph{}
	g.node("local").Set("k", String("only on the edge"), float64(time.Now().UnixMilli()))
	if _, err := edge.apply(ctx, g, nil); err != nil {
		t.Fatal(err)
	}
	if v, err := backup.Get("local").Get("k").Once[string](ctx); err != nil || v != "only on the edge" {
		t.Fatalf("the backup reading the edge: %q, %v", v, err)
	}
}

// TestWriteToQueuesWhileAway checks that writes for a peer we only write
// to wait for it, like those for any peer.
func TestWriteToQueuesWhileAway(t *testing.T) {
	ctx := t.Context()
	backup := newDB(t)
	srv := httptest.NewUnstartedServer(backup)
	edge := New(Options{Peers: []string{WriteTo("ws://" + srv.Listener.Addr().String() + "/gun")}})
	defer edge.Close()
	edge.Get("orders").Get("1").Put(ctx, map[string]int{"qty": 1}) // nobody there yet
	srv.Start()
	defer srv.Close()
	eventually(t, func() bool { return stored(t, backup, "orders/1") })
}

func TestPeerModeStrings(t *testing.T) {
	for _, tc := range []struct {
		in   string
		url  string
		mode PeerMode
	}{
		{"ws://a/gun", "ws://a/gun", PeerBoth},
		{"ws://a/gun?token=x", "ws://a/gun?token=x", PeerBoth},
		{"ws://a/gun?gundb_mode=read", "ws://a/gun", PeerRead},
		{"ws://a/gun?gundb_mode=WRITE", "ws://a/gun", PeerWrite},
		{"ws://a/gun?gundb_mode=both", "ws://a/gun", PeerBoth},
		{"wss://a/gun?token=x&gundb_mode=read&v=2", "wss://a/gun?token=x&v=2", PeerRead},
		{"wss://a/gun?gundb_mode=write#f", "wss://a/gun#f", PeerWrite},
		{ReadFrom("ws://a/gun"), "ws://a/gun", PeerRead},
		{WriteTo("wss://a/gun?token=x"), "wss://a/gun?token=x", PeerWrite},
		{ReadFrom("ws://a/gun?gundb_mode=write"), "ws://a/gun", PeerRead}, // the helper wins
	} {
		if url, mode, err := ParsePeer(tc.in); err != nil || url != tc.url || mode != tc.mode {
			t.Errorf("ParsePeer(%q) = %q, %v, %v; want %q, %v", tc.in, url, mode, err, tc.url, tc.mode)
		}
	}
	for _, bad := range []string{"ws://a/gun?gundb_mode=reed", "ws://a/gun?gundb_mode=", "ws://a/gun?gundb_mode=read&gundb_mode=write"} {
		if _, _, err := ParsePeer(bad); err == nil {
			t.Errorf("ParsePeer(%q) took a bad mode", bad)
		}
	}
	if ReadFrom("ws://a/gun") != "ws://a/gun?gundb_mode=read" || WriteTo("ws://a/gun?t=1") != "ws://a/gun?t=1&gundb_mode=write" {
		t.Errorf("helpers: %s, %s", ReadFrom("ws://a/gun"), WriteTo("ws://a/gun?t=1"))
	}
	if PeerBoth.String() != "both" || PeerRead.String() != "read" || PeerWrite.String() != "write" {
		t.Error("PeerMode names")
	}
}

// TestBadModeIsNotDialled checks that a typo in the mode fails closed.
func TestBadModeIsNotDialled(t *testing.T) {
	primary := newDB(t)
	db := New(Options{Peers: []string{serve(t, primary) + "?gundb_mode=reed"}})
	defer db.Close()
	time.Sleep(200 * time.Millisecond)
	if len(primary.Peers()) != 0 {
		t.Fatal("a peer with a bad mode was dialled")
	}
}

// TestModeParamNotSent checks the server never sees ?gundb_mode=.
func TestModeParamNotSent(t *testing.T) {
	primary := newDB(t)
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.URL.RawQuery:
		default:
		}
		primary.ServeHTTP(w, r)
	}))
	defer srv.Close()
	db := New(Options{Peers: []string{srv.URL + "/gun?token=x&gundb_mode=read"}})
	defer db.Close()
	select {
	case q := <-got:
		if q != "token=x" {
			t.Fatalf("the server saw the query %q, want token=x", q)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("never dialled")
	}
}

func TestPeerInfoHasTheMode(t *testing.T) {
	primary := newDB(t)
	url := serve(t, primary)
	got := make(chan PeerInfo, 1)
	db := New(Options{
		Peers:     []string{ReadFrom(url)},
		OnConnect: func(_ context.Context, p PeerInfo) { got <- p },
	})
	defer db.Close()
	select {
	case p := <-got:
		if p.URL != url || p.Mode != PeerRead {
			t.Fatalf("PeerInfo %+v, want URL %s and PeerRead", p, url)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no OnConnect")
	}
}

