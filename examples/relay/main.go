// A GUN relay server. JS and Go peers connect to ws://localhost:8765/gun.
//
//	go run ./examples/relay
//
// In the browser: Gun(['http://localhost:8765/gun'])
package main

import (
	"flag"
	"log"
	"net/http"

	"ella.to/gundb"
)

func main() {
	addr := flag.String("addr", ":8765", "listen address")
	flag.Parse()

	db := gundb.New()
	defer db.Close()

	http.Handle("/gun", db)
	log.Printf("GUN relay on ws://localhost%s/gun", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
