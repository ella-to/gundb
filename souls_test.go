package gundb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestGetSoulLex(t *testing.T) {
	for in, want := range map[string]string{
		`{"#":{"*":"chat/"}}`:                `{"#":{"*":"chat/"},"%":100000}`,
		`{"#":{">":"a","<":"b"},".":"name"}`: `{"#":{">":"a","<":"b"},"%":100000}`, // "." is ignored, like radisk
		`{"#":{"=":"mark"}}`:                 `{"#":"mark"}`,
		`{"#":5}`:                            `{"#":""}`,
	} {
		var q getQuery
		if err := json.Unmarshal([]byte(in), &q); err != nil {
			t.Fatal(err)
		}
		q.raw = nil // re-encode from the parsed form
		if b, _ := marshalWire(&q); string(b) != want {
			t.Errorf("%s: got %s, want %s", in, b, want)
		}
	}
}

func TestLexMatchSoul(t *testing.T) {
	cases := []struct {
		q    Lex
		soul string
		want bool
	}{
		{Lex{}, "anything", true},
		{Lex{Prefix: "chat/"}, "chat/1", true},
		{Lex{Prefix: "chat/"}, "chats", false},
		{Lex{From: "b", To: "d"}, "b", true},
		{Lex{From: "b", To: "d"}, "c9", true},
		{Lex{From: "b", To: "d"}, "d", true},
		{Lex{From: "b", To: "d"}, "d0", false},
		{Lex{From: "b"}, "zz", true},
		{Lex{To: "b"}, "a", true},
		{Lex{Prefix: "chat/", From: "chat/2", To: "chat/3"}, "chat/25", true},
		{Lex{Prefix: "chat/", From: "chat/2", To: "chat/3"}, "chat/1", false},
		{Lex{Prefix: "x", From: "a"}, "b", false}, // both rules apply, unlike String.match
		{Lex{From: "￿"}, "\U0001F600", false},     // JS (UTF-16) order, not UTF-8
	}
	for _, c := range cases {
		if got := c.q.lex().matchSoul(c.soul); got != c.want {
			t.Errorf("%+v matchSoul(%q) = %v", c.q, c.soul, got)
		}
	}
}

func TestSoulsLocal(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	for _, s := range []string{"chat/2", "chat/10", "chat/1", "chats", "other"} {
		db.Get(s).Put(ctx, map[string]any{"v": s})
	}
	souls, err := db.Souls(ctx, Lex{Prefix: "chat/"})
	if err != nil || !slices.Equal(souls, []string{"chat/1", "chat/10", "chat/2"}) {
		t.Fatalf("%q %v", souls, err)
	}
	souls, _ = db.Souls(ctx, Lex{From: "chat/10", To: "chats"})
	if !slices.Equal(souls, []string{"chat/10", "chat/2", "chats"}) {
		t.Fatalf("range: %q", souls)
	}
}

type plainStore struct{ Store }

func TestSoulsNeedsSoulStore(t *testing.T) {
	db := New(Options{Store: plainStore{NewMemoryStore()}})
	defer db.Close()
	if _, err := db.Souls(t.Context(), Lex{}); !errors.Is(err, ErrNoSoulStore) {
		t.Fatalf("got %v", err)
	}
}

func TestSoulsFromPeers(t *testing.T) {
	// alice <-> relay <-> bob: alice's nodes reach bob through the relay,
	// and bob is then subscribed to them.
	ctx := t.Context()
	relay, alice, bob := newDB(t), newDB(t), newDB(t)
	link(t, relay, alice)
	link(t, relay, bob)
	for i := range 3 {
		alice.Get(fmt.Sprintf("msg/%d", i)).PutAck(ctx, map[string]any{"text": fmt.Sprint("hi ", i)})
	}
	alice.Get("other").PutAck(ctx, map[string]any{"x": 1})

	souls, err := bob.Souls(ctx, Lex{Prefix: "msg/"})
	if err != nil || !slices.Equal(souls, []string{"msg/0", "msg/1", "msg/2"}) {
		t.Fatalf("%q %v", souls, err)
	}
	if n, _ := bob.store.Get(ctx, "other"); n != nil {
		t.Fatal("bob got a node outside the query")
	}
	text, err := bob.Get("msg/1").Get("text").Once[string](ctx)
	if err != nil || text != "hi 1" {
		t.Fatalf("%q %v", text, err)
	}
	alice.Get("msg/1").Get("text").Put(ctx, "edited")
	eventually(t, func() bool {
		n, _ := bob.store.Get(ctx, "msg/1")
		return n != nil && n.Fields["text"] == String("edited")
	})
}

func TestSoulLexRepliesAreChunked(t *testing.T) {
	ctx := t.Context()
	relay := newDB(t)
	for i := range lexChunk + 5 {
		relay.Get(fmt.Sprintf("n/%03d", i)).Put(ctx, map[string]any{"i": i})
	}
	conn := newScriptConn()
	go relay.Attach(conn)
	conn.recv <- []byte(`{"#":"q1","get":{"#":{"*":"n/"}}}`)
	var nodes, chunks int
	for last := false; !last; {
		var frame []byte
		select {
		case frame = <-conn.sent:
		case <-time.After(3 * time.Second):
			t.Fatalf("reply incomplete: %d nodes in %d chunks", nodes, chunks)
		}
		msgs, _ := readFrame(frame) // replies may be batched into one frame
		for _, m := range msgs {
			if m.Ack == "q1" {
				chunks++
				nodes += len(m.Put)
				last = len(m.More) == 0
			}
		}
	}
	if chunks != 2 || nodes != lexChunk+5 {
		t.Fatalf("%d nodes in %d chunks", nodes, chunks)
	}
}

func TestSoulLexHonoursCanRead(t *testing.T) {
	ctx := context.Background()
	relay, url := authRelay(t)
	ali := client(t, relay, url, "tok-ali")
	ali.Get("secrets").Get("ali").Get("pin").PutAck(ctx, "1234")
	ali.Get("public").Get("v").PutAck(ctx, "news")

	bob := client(t, relay, url, "tok-bob")
	souls, err := bob.Souls(ctx, Lex{})
	if err != nil || slices.Contains(souls, "secrets/ali") || !slices.Contains(souls, "public") {
		t.Fatalf("%q %v", souls, err)
	}
}
