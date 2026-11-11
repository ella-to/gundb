package gundb

import (
	"context"
	"strings"
	"testing"
	"time"
)

// routePeer is a peer of db that records what it is sent. With dialled it
// is one db connected to (Options.Peers), else one that connected to db.
func routePeer(t *testing.T, db *DB, dialled bool) (*peer, *fakeConn) {
	t.Helper()
	conn := newFakeConn(false)
	ctx, cancel := context.WithCancel(db.ctx)
	t.Cleanup(cancel)
	p := &peer{db: db, conn: conn, ctx: ctx, cancel: cancel, id: randomID(16), outWake: make(chan struct{}, 1), wants: map[string]bool{}}
	go p.writeLoop()
	db.peersMu.Lock()
	db.peers[p] = struct{}{}
	if dialled {
		o := &outbound{url: "ws://upstream", mode: PeerBoth, live: p}
		p.origin = o
		db.outbound = append(db.outbound, o)
	}
	db.peersMu.Unlock()
	return p, conn
}

// sentWithin returns the frames conn is sent within d.
func sentWithin(conn *fakeConn, d time.Duration) string {
	var out []string
	deadline := time.After(d)
	for {
		select {
		case f := <-conn.frames:
			out = append(out, string(f))
		case <-deadline:
			return strings.Join(out, "\n")
		}
	}
}

func TestRouteByInterest(t *testing.T) {
	db := New(Options{RouteByInterest: true, Wait: 100 * time.Millisecond})
	defer db.Close()
	upstream, upConn := routePeer(t, db, true)
	asker, askConn := routePeer(t, db, false)
	watcher, watchConn := routePeer(t, db, false)
	bystander, byConn := routePeer(t, db, false)
	_, _ = upstream, bystander
	watcher.want("chat")

	// A get the DB cannot answer goes upstream, and to whoever asked for
	// the same node, not to every client.
	msgs, err := readFrame([]byte(`{"#":"q1","get":{"#":"chat"}}`))
	if err != nil {
		t.Fatal(err)
	}
	db.handle(asker, &msgs[0])
	if got := sentWithin(upConn, 50*time.Millisecond); !strings.Contains(got, `"q1"`) {
		t.Fatalf("upstream was not asked: %s", got)
	}
	if got := sentWithin(watchConn, 30*time.Millisecond); !strings.Contains(got, `"q1"`) {
		t.Fatalf("the peer that asked for chat was not asked: %s", got)
	}
	if got := sentWithin(byConn, 30*time.Millisecond); got != "" {
		t.Fatalf("a bystander was asked: %s", got)
	}
	// Nobody answers: the asker hears "not found" once Wait has passed.
	if got := sentWithin(askConn, 300*time.Millisecond); !strings.Contains(got, `"@":"q1"`) {
		t.Fatalf("no answer to the asker: %s", got)
	}

	// The DB's own write goes upstream and to the peer that asked for it.
	if err := db.Get("chat").Put(t.Context(), map[string]any{"last": "hi"}); err != nil {
		t.Fatal(err)
	}
	if got := sentWithin(upConn, 50*time.Millisecond); !strings.Contains(got, `"hi"`) {
		t.Fatalf("upstream did not get the write: %s", got)
	}
	if got := sentWithin(watchConn, 30*time.Millisecond); !strings.Contains(got, `"hi"`) {
		t.Fatalf("the watcher did not get the write: %s", got)
	}
	if got := sentWithin(byConn, 30*time.Millisecond); strings.Contains(got, `"hi"`) {
		t.Fatalf("a bystander got the write: %s", got)
	}
}

// Without RouteByInterest a miss is passed on to every peer, and a
// relayed get still finishes when its peers answer.
func TestRelayGetAsksEveryPeerByDefault(t *testing.T) {
	db := New(Options{Wait: time.Second})
	defer db.Close()
	asker, askConn := routePeer(t, db, false)
	other, otherConn := routePeer(t, db, false)
	msgs, _ := readFrame([]byte(`{"#":"q1","get":{"#":"chat"}}`))
	db.handle(asker, &msgs[0])
	if got := sentWithin(otherConn, 50*time.Millisecond); !strings.Contains(got, `"q1"`) {
		t.Fatalf("the other client was not asked: %s", got)
	}
	start := time.Now()
	reply, _ := readFrame([]byte(`{"#":"r1","@":"q1","put":{"chat":{"_":{"#":"chat",">":{"last":1}},"last":"yo"}}}`))
	db.handle(other, &reply[0])
	if got := sentWithin(askConn, 100*time.Millisecond); !strings.Contains(got, `"yo"`) {
		t.Fatalf("the answer was not passed on: %s", got)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("the request waited for its timeout")
	}
	db.reqMu.Lock()
	defer db.reqMu.Unlock()
	if len(db.pending) != 0 {
		t.Fatalf("%d requests still pending", len(db.pending))
	}
}
