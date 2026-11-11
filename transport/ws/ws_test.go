package ws_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ella.to/gundb"
	"ella.to/gundb/transport"
	"ella.to/gundb/transport/ws"
)

func TestSyncOverWebSocket(t *testing.T) {
	ctx := t.Context()
	server := gundb.New()
	defer server.Close()
	srv := httptest.NewServer(server) // a DB is an http.Handler
	defer srv.Close()

	url := strings.Replace(srv.URL, "http://", "ws://", 1) + "/gun"
	alice := gundb.New(gundb.Options{Peers: []string{url}})
	bob := gundb.New(gundb.Options{Peers: []string{srv.URL + "/gun"}}) // http:// works too
	defer alice.Close()
	defer bob.Close()

	got := make(chan string, 10)
	off := bob.Get("chat").Get("last").On(func(s string) { got <- s })
	defer off()
	time.Sleep(100 * time.Millisecond)

	if err := alice.Get("chat").Get("last").PutAck(ctx, "hi bob"); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-got:
		if s != "hi bob" {
			t.Fatalf("got %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bob never got the message")
	}

	carol := gundb.New(gundb.Options{Peers: []string{url}})
	defer carol.Close()
	v, err := carol.Get("chat").Get("last").Once[string](ctx)
	if err != nil || v != "hi bob" {
		t.Fatalf("late joiner: %q %v", v, err)
	}
}

func TestSyncCompressed(t *testing.T) {
	ctx := t.Context()
	server := gundb.New(gundb.Options{Compress: true})
	defer server.Close()
	srv := httptest.NewServer(server)
	defer srv.Close()
	client := gundb.New(gundb.Options{Compress: true, Peers: []string{srv.URL + "/gun"}})
	defer client.Close()
	big := strings.Repeat("compress me ", 1000)
	if err := client.Get("doc").Get("text").PutAck(ctx, big); err != nil {
		t.Fatal(err)
	}
	other := gundb.New(gundb.Options{Peers: []string{srv.URL + "/gun"}}) // offers no compression
	defer other.Close()
	if got, err := other.Get("doc").Get("text").Once[string](ctx); err != nil || got != big {
		t.Fatalf("read back %d bytes, %v", len(got), err)
	}
}

// Frames from Recv are the caller's: a later frame must not change them.
func TestRecvFramesDoNotAlias(t *testing.T) {
	ctx := t.Context()
	conns := make(chan transport.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := ws.Accept(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		conns <- c // the connection is hijacked: it outlives the handler
	}))
	defer srv.Close()
	client, err := ws.Dialer{}.Dial(ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	server := <-conns
	defer func() {
		go server.Recv(ctx) // reads the close frame, so the close handshake ends at once
		client.Close()
		server.Close()
	}()
	var got [][]byte
	for _, f := range []string{`{"#":"first"}`, `{"#":"second, longer"}`, `{"#":"3"}`} {
		if err := client.Send(ctx, []byte(f)); err != nil {
			t.Fatal(err)
		}
		b, err := server.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, b)
	}
	if string(got[0]) != `{"#":"first"}` || string(got[1]) != `{"#":"second, longer"}` || string(got[2]) != `{"#":"3"}` {
		t.Fatalf("frames changed: %q", got)
	}
}
