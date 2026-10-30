// A large list: show the newest 10 of 1,000 messages without loading them all.
package main

import (
	"context"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"slices"
	"time"

	"ella.to/gundb"
)

type Message struct {
	Text string `json:"text"`
}

func main() {
	ctx := context.Background()

	// A relay that already holds a long feed.
	relay := gundb.New()
	defer relay.Close()
	for i := range 1000 {
		_, err := relay.Get("feed").Set(ctx, Message{Text: fmt.Sprint("message ", i+1)})
		check(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	go http.Serve(l, relay)
	peer := "ws://" + l.Addr().String() + "/gun"

	// The slow way: Once loads every message, just to show 10 of them.
	a := gundb.New(gundb.Options{Peers: []string{peer}})
	defer a.Close()
	start := time.Now()
	all, err := a.Get("feed").Once[map[string]Message](ctx)
	check(err)
	fmt.Printf("Once:  loaded %d messages in %v\n", len(all), time.Since(start).Round(time.Millisecond))

	// The fast way: Links reads only the list node, key -> soul.
	b := gundb.New(gundb.Options{Peers: []string{peer}})
	defer b.Close()
	start = time.Now()
	links, err := b.Get("feed").Links(ctx)
	check(err)

	// Set souls start with the time they were made, so sorting them sorts
	// the messages oldest to newest. Take the last 10 and load only those.
	souls := slices.Sorted(maps.Values(links))
	newest := souls[max(0, len(souls)-10):]
	var page []Message
	for _, soul := range slices.Backward(newest) {
		m, err := b.Get(soul).Once[Message](ctx)
		check(err)
		page = append(page, m)
	}
	fmt.Printf("Links: %d messages in the feed, newest %d loaded in %v\n", len(links), len(page), time.Since(start).Round(time.Millisecond))
	for _, m := range page {
		fmt.Println(" ", m.Text)
	}
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
