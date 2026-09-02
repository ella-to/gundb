// A command-line client for any GUN peer, Go or JS.
//
//	go run ./examples/client put  users.alice.name '"Alice"'
//	go run ./examples/client get  users.alice
//	go run ./examples/client watch users.alice
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"

	"ella.to/gundb"
)

func main() {
	peer := flag.String("peer", "ws://localhost:8765/gun", "GUN peer URL")
	flag.Parse()
	cmd, path, value := flag.Arg(0), flag.Arg(1), flag.Arg(2)
	if path == "" {
		log.Fatal("usage: client [-peer url] get|put|watch a.b.c [json]")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	db := gundb.New(gundb.Options{Peers: []string{*peer}})
	defer db.Close()

	keys := strings.Split(path, ".")
	ref := db.Get(keys[0])
	for _, k := range keys[1:] {
		ref = ref.Get(k)
	}

	switch cmd {
	case "get":
		v, err := ref.Once[any](ctx)
		if err != nil {
			log.Fatal(err)
		}
		show(v)
	case "put":
		var v any
		if err := json.Unmarshal([]byte(value), &v); err != nil {
			v = value // not JSON: store as a string
		}
		if err := ref.PutAck(ctx, v); err != nil {
			log.Fatal(err)
		}
		fmt.Println("ok")
	case "watch":
		defer ref.On(show)()
		<-ctx.Done()
	default:
		log.Fatalf("unknown command %q", cmd)
	}
}

func show(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}
