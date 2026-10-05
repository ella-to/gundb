// Offline-first: devices keep reading and writing while the relay is down,
// and everything syncs once it is back. Takes a few seconds: peers are
// re-dialled every 2s.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"ella.to/gundb"
)

type Note struct {
	Title  string `json:"title,omitempty"`
	Body   string `json:"body,omitempty"`
	Status string `json:"status,omitempty"`
}

func main() {
	ctx := context.Background()

	// Pick the relay's address now; nothing listens on it yet.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	addr := l.Addr().String()
	l.Close()
	url := "ws://" + addr + "/gun"

	// Two devices configured with the relay. They start offline.
	phone := gundb.New(gundb.Options{Peers: []string{url}})
	laptop := gundb.New(gundb.Options{Peers: []string{url}})
	defer phone.Close()
	defer laptop.Close()

	// 1. Offline: writes are saved locally and queued for the relay.
	note := phone.Get("notes").Get("trip")
	check(note.Put(ctx, Note{Title: "Trip", Status: "draft"}))
	err = note.PutAck(ctx, Note{Body: "pack light"})
	fmt.Println("ack while offline:", errors.Is(err, gundb.ErrNoPeers)) // still saved and queued
	n, err := note.Once[Note](ctx)
	check(err)
	fmt.Printf("phone reads offline: %s, %q\n", n.Title, n.Body) // phone reads offline: Trip, "pack light"

	// 2. The relay comes up. The phone reconnects and sends its queue;
	// the laptop, which never saw the note, gets it from the relay.
	store := gundb.NewMemoryStore() // shared by every relay run, like a disk
	stop := startRelay(addr, store)
	waitOnline(phone, laptop)
	n, err = laptop.Get("notes").Get("trip").Once[Note](ctx)
	check(err)
	fmt.Printf("laptop reads: %s, %q\n", n.Title, n.Body) // laptop reads: Trip, "pack light"

	// 3. The relay goes down. Both devices edit the note while offline,
	// including the same field.
	stop()
	waitOffline(phone, laptop)
	check(phone.Get("notes").Get("trip").Get("body").Put(ctx, "pack light, bring a map"))
	check(phone.Get("notes").Get("trip").Get("status").Put(ctx, "ready"))
	time.Sleep(10 * time.Millisecond) // the laptop's edit is the later one
	check(laptop.Get("notes").Get("trip").Get("status").Put(ctx, "booked"))

	// 4. Back online: queued writes cross over, and HAM settles the
	// conflict the same way on every peer (the later write wins).
	stop = startRelay(addr, store)
	defer stop()
	waitOnline(phone, laptop)
	for _, d := range []struct {
		name string
		db   *gundb.DB
	}{{"phone", phone}, {"laptop", laptop}} {
		n := waitFor(d.db, func(n Note) bool { return n.Status == "booked" && n.Body == "pack light, bring a map" })
		fmt.Printf("%s after resync: %s, %q, %s\n", d.name, n.Title, n.Body, n.Status)
	}
	// phone after resync: Trip, "pack light, bring a map", booked
	// laptop after resync: Trip, "pack light, bring a map", booked
}

// startRelay serves a relay on addr until stop is called. Closing the DB
// drops every connection, as a crashed or restarted server would.
func startRelay(addr string, store gundb.Store) (stop func()) {
	relay := gundb.New(gundb.Options{Store: store})
	l, err := net.Listen("tcp", addr)
	check(err)
	srv := &http.Server{Handler: relay}
	go srv.Serve(l)
	return func() {
		srv.Close()
		relay.Close()
	}
}

func waitOnline(dbs ...*gundb.DB) {
	until(func() bool {
		for _, db := range dbs {
			if len(db.Peers()) == 0 {
				return false
			}
		}
		return true
	})
}

func waitOffline(dbs ...*gundb.DB) {
	until(func() bool {
		for _, db := range dbs {
			if len(db.Peers()) > 0 {
				return false
			}
		}
		return true
	})
}

// waitFor waits until the note on db satisfies ok. Updates from peers
// arrive in the background; On delivers each one.
func waitFor(db *gundb.DB, ok func(Note) bool) Note {
	got := make(chan Note, 16)
	off := db.Get("notes").Get("trip").On(func(n Note) { got <- n })
	defer off()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case n := <-got:
			if ok(n) {
				return n
			}
		case <-timeout:
			log.Fatal("peers did not converge")
		}
	}
}

func until(ok func() bool) {
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			log.Fatal("timed out")
		}
	}
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
