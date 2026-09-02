// Three peers syncing through a relay, all in one program.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"

	"ella.to/gundb"
)

func main() {
	ctx := context.Background()

	// The relay: a DB is an http.Handler speaking GUN over WebSocket.
	relay := gundb.New()
	defer relay.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	go http.Serve(l, relay)
	peer := "ws://" + l.Addr().String() + "/gun"

	alice := gundb.New(gundb.Options{Peers: []string{peer}})
	bob := gundb.New(gundb.Options{Peers: []string{peer}})
	defer alice.Close()
	defer bob.Close()

	// Bob listens...
	got := make(chan string)
	off := bob.Get("chat").Get("last").On(func(msg string) { got <- msg })
	defer off()

	// ...Alice writes, and waits for the relay to confirm.
	check(alice.Get("chat").Get("last").PutAck(ctx, "hi bob!"))
	fmt.Println("bob got:", <-got)

	// A peer joining later reads the data from the network.
	carol := gundb.New(gundb.Options{Peers: []string{peer}})
	defer carol.Close()
	msg, err := carol.Get("chat").Get("last").Once[string](ctx)
	check(err)
	fmt.Println("carol read:", msg)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
