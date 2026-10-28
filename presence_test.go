package gundb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// events records OnConnect and OnDisconnect calls.
type events struct {
	mu      sync.Mutex
	up      []PeerInfo
	down    []PeerInfo
	balance bool // an OnDisconnect never came before its OnConnect
}

func (e *events) connect(p PeerInfo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.up = append(e.up, p)
}

func (e *events) disconnect(p PeerInfo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	found := false
	for _, u := range e.up {
		found = found || u.PeerID == p.PeerID
	}
	e.balance = e.balance || !found
	e.down = append(e.down, p)
}

func (e *events) counts() (up, down int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.up), len(e.down)
}

func TestOnConnectReportsUserAndCustomID(t *testing.T) {
	var server events
	relay := New(Options{
		Authenticate: func(r *http.Request) (string, string, error) {
			return r.URL.Query().Get("user"), r.URL.Query().Get("device"), nil
		},
		OnConnect:    server.connect,
		OnDisconnect: server.disconnect,
	})
	defer relay.Close()
	srv := httptest.NewServer(relay)
	defer srv.Close()

	// Two tabs on one device, and a second device.
	var tab1 events
	url1 := srv.URL + "/gun?user=ali&device=laptop"
	a := New(Options{Peers: []string{url1}, OnConnect: tab1.connect, OnDisconnect: tab1.disconnect})
	defer a.Close()
	b := New(Options{Peers: []string{srv.URL + "/gun?user=ali&device=laptop"}})
	defer b.Close()
	c := New(Options{Peers: []string{srv.URL + "/gun?user=ali&device=phone"}})
	defer c.Close()
	eventually(t, func() bool { up, _ := server.counts(); return up == 3 })

	server.mu.Lock()
	ids, devices := map[string]bool{}, map[string]int{}
	for _, p := range server.up {
		if p.User != "ali" || p.URL != "" || p.Remote == "" {
			t.Errorf("server saw %+v", p)
		}
		ids[p.PeerID] = true
		devices[p.CustomID]++
	}
	server.mu.Unlock()
	if len(ids) != 3 {
		t.Errorf("PeerIDs not unique: %v", ids)
	}
	if devices["laptop"] != 2 || devices["phone"] != 1 {
		t.Errorf("devices = %v", devices)
	}

	// The dialling side knows which configured peer came up.
	eventually(t, func() bool { up, _ := tab1.counts(); return up == 1 })
	if p := tab1.up[0]; p.URL != url1 || p.PeerID == "" {
		t.Errorf("client saw %+v", p)
	}

	// Closing a tab is reported with the same PeerInfo it connected with.
	b.Close()
	eventually(t, func() bool { _, down := server.counts(); return down == 1 })
	server.mu.Lock()
	if p := server.down[0]; p.CustomID != "laptop" || !ids[p.PeerID] {
		t.Errorf("disconnect %+v", p)
	}
	server.mu.Unlock()

	// The relay going away is reported to the client.
	relay.Close()
	eventually(t, func() bool { _, down := tab1.counts(); return down >= 1 })
	server.mu.Lock()
	tab1.mu.Lock()
	defer server.mu.Unlock()
	defer tab1.mu.Unlock()
	if server.balance || tab1.balance {
		t.Error("OnDisconnect without OnConnect")
	}
}

func TestOnConnectAgainAfterReconnect(t *testing.T) {
	old := redialEvery
	redialEvery = 20 * time.Millisecond
	defer func() { redialEvery = old }()

	relay := newDB(t)
	srv := httptest.NewServer(relay)
	defer srv.Close()
	var got events
	db := New(Options{Peers: []string{srv.URL + "/gun"}, OnConnect: got.connect, OnDisconnect: got.disconnect})
	defer db.Close()
	eventually(t, func() bool { up, _ := got.counts(); return up == 1 })

	// Drop the connection from the relay's side; the client redials.
	relay.peersMu.Lock()
	for p := range relay.peers {
		p.close()
	}
	relay.peersMu.Unlock()
	eventually(t, func() bool { up, down := got.counts(); return up == 2 && down == 1 })
	if got.up[0].PeerID == got.up[1].PeerID {
		t.Error("a new connection kept the old PeerID")
	}
}

// silentConn is a peer that vanished without closing: pings never return.
type silentConn struct{ *scriptConn }

func (silentConn) Ping(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

func TestPeerThatStopsAnsweringIsDropped(t *testing.T) {
	oldBeat, oldWait := heartbeatEvery, pongWait
	heartbeatEvery, pongWait = 20*time.Millisecond, 20*time.Millisecond
	defer func() { heartbeatEvery, pongWait = oldBeat, oldWait }()

	var got events
	relay := New(Options{OnConnect: got.connect, OnDisconnect: got.disconnect})
	defer relay.Close()
	go relay.AttachAs(silentConn{newScriptConn()}, "ali", "phone")
	eventually(t, func() bool { _, down := got.counts(); return down == 1 })
	if p := got.down[0]; p.User != "ali" || p.CustomID != "phone" {
		t.Errorf("disconnect %+v", p)
	}
}

func TestPeerThatAnswersPingsStays(t *testing.T) {
	oldBeat, oldWait := heartbeatEvery, pongWait
	heartbeatEvery, pongWait = 10*time.Millisecond, 50*time.Millisecond
	defer func() { heartbeatEvery, pongWait = oldBeat, oldWait }()

	var got events
	a := New(Options{OnDisconnect: got.disconnect})
	defer a.Close()
	b := newDB(t)
	link(t, a, b)
	time.Sleep(200 * time.Millisecond) // many ping rounds
	if _, down := got.counts(); down != 0 || len(a.Peers()) != 1 {
		t.Fatalf("live peer dropped: %d disconnects, %d peers", down, len(a.Peers()))
	}
}
