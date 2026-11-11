package gundb

import (
	"context"
	"strconv"
	"testing"
)

// Reading a path through a large node fetches one field of it from peers,
// not the whole node; reading the node later fetches the rest.
func TestPathReadsFetchOneField(t *testing.T) {
	ctx := t.Context()
	relay := newDB(t)
	fields := map[string]any{}
	for i := range 100 {
		fields["k"+strconv.Itoa(i)] = map[string]any{"v": i}
	}
	if err := relay.Get("list").Put(ctx, fields); err != nil {
		t.Fatal(err)
	}
	clear(relay.interest) // or the relay offers the client what it wrote
	client := newDB(t)
	link(t, relay, client)

	v, err := client.Get("list").Get("k7").Get("v").Once[int](ctx)
	if err != nil || v != 7 {
		t.Fatalf("list.k7.v = %v, %v", v, err)
	}
	n, err := client.store.Get(ctx, "list")
	if err != nil || n == nil || len(n.Fields) != 1 || n.Fields["k7"] == nil {
		t.Fatalf("client holds %+v (%v), want only list.k7", n, err)
	}

	all, err := client.Get("list").Once[map[string]struct{ V int }](ctx)
	if err != nil || len(all) != 100 || all["k42"].V != 42 {
		t.Fatalf("whole list: %d items, %v", len(all), err)
	}
}

// A field asked for alone is asked for again, alone, from a new peer.
func TestResubscribeAsksForFields(t *testing.T) {
	db := newDB(t)
	db.interest[interestKey{"list", "k7"}] = &interest{done: make(chan struct{})}
	conn := newFakeConn(false)
	pctx, cancel := context.WithCancel(db.ctx)
	defer cancel()
	p := &peer{db: db, conn: conn, ctx: pctx, cancel: cancel, outWake: make(chan struct{}, 1), wants: map[string]bool{}}
	go p.writeLoop()
	db.resubscribe(p)
	msgs, err := readFrame(<-conn.frames)
	if err != nil || len(msgs) != 1 || msgs[0].Get == nil || msgs[0].Get.Soul != "list" || msgs[0].Get.Key != "k7" {
		t.Fatalf("resubscribe sent %+v (%v)", msgs, err)
	}
}

// A put of many nested objects reads the node once, and still writes into
// the nodes their fields already link to.
func TestPutManyNestedKeepsLinks(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	list := db.Get("list")
	souls := map[string]string{}
	for i := range 20 {
		item, err := list.Set(ctx, map[string]any{"n": i})
		if err != nil {
			t.Fatal(err)
		}
		souls[item.Key()] = item.Key()
	}
	update := map[string]any{}
	for soul := range souls {
		update[soul] = map[string]any{"done": true}
	}
	if err := list.Put(ctx, update); err != nil {
		t.Fatal(err)
	}
	links, err := list.Links(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for key, soul := range links {
		if souls[key] != soul {
			t.Fatalf("%s now links to %s", key, soul)
		}
		done, err := db.Get(soul).Get("done").Once[bool](ctx)
		if err != nil || !done {
			t.Fatalf("%s.done = %v, %v", soul, done, err)
		}
	}
}

// A node larger than one message is sent in chunks, also through a relay
// that passes on what it does not have, and is read back whole.
func TestLargeNodeInChunks(t *testing.T) {
	ctx := t.Context()
	origin := newDB(t)
	fields := map[string]any{}
	for i := range 2500 {
		fields["k"+strconv.Itoa(i)] = i
	}
	if err := origin.Get("big").Put(ctx, fields); err != nil {
		t.Fatal(err)
	}
	clear(origin.interest)

	direct := newDB(t)
	link(t, origin, direct)
	got, err := direct.Get("big").Once[map[string]int](ctx)
	if err != nil || len(got) != 2500 || got["k2499"] != 2499 {
		t.Fatalf("read %d fields directly, %v", len(got), err)
	}

	relay := newDB(t)
	link(t, origin, relay) // the relay dials origin
	clear(relay.interest)
	client := newDB(t)
	link(t, relay, client)
	got, err = client.Get("big").Once[map[string]int](ctx)
	if err != nil || len(got) != 2500 || got["k0"] != 0 {
		t.Fatalf("read %d fields through a relay, %v", len(got), err)
	}
}
