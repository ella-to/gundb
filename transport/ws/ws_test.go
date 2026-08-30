package ws_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ella.to/gundb"
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
