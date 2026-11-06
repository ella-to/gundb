// One-way peers: a read replica that follows a primary without ever
// writing to it, and a backup that gets every write but is never read.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"ella.to/gundb"
)

// serve makes db a relay on a free port and returns its URL.
func serve(db *gundb.DB) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	go http.Serve(l, db)
	return "ws://" + l.Addr().String() + "/gun"
}

func main() {
	ctx := context.Background()

	backup := gundb.New()
	defer backup.Close()

	// The primary syncs with nobody but the backup, and only writes to it.
	primary := gundb.New(gundb.Options{Peers: []string{gundb.WriteTo(serve(backup))}})
	defer primary.Close()

	// The replica follows the primary, and only reads from it.
	replica := gundb.New(gundb.Options{Peers: []string{gundb.ReadFrom(serve(primary))}})
	defer replica.Close()

	// A write on the primary reaches the backup, and the replica reads it.
	if err := primary.Get("prices").Get("milk").PutAck(ctx, 2); err != nil {
		log.Fatal(err)
	}
	milk, err := replica.Get("prices").Get("milk").Once[int](ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("replica reads milk:", milk)

	// The replica's own writes stay on the replica.
	err = replica.Get("prices").Get("milk").PutAck(ctx, 0)
	fmt.Println("replica's PutAck:", err) // no peer to write to
	time.Sleep(100 * time.Millisecond)
	milk, _ = primary.Get("prices").Get("milk").Once[int](ctx)
	fmt.Println("primary still has milk:", milk)

	// The backup has everything; what it writes never comes back.
	milk, _ = backup.Get("prices").Get("milk").Once[int](ctx)
	fmt.Println("backup has milk:", milk)
	backup.Get("prices").Get("milk").Put(ctx, 99)
	time.Sleep(100 * time.Millisecond)
	milk, _ = primary.Get("prices").Get("milk").Once[int](ctx)
	fmt.Println("primary after the backup wrote 99:", milk)
}
