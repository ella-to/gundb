# gundb guide

Every code block below is a complete program from [`examples/`](examples/)
(a test keeps them identical). Run any of them with `go run ./examples/<name>`.

```sh
go get ella.to/gundb   # needs Go 1.27+
```

## JS → Go cheat sheet

| GUN (JavaScript)                         | gundb (Go)                                          |
| ---------------------------------------- | --------------------------------------------------- |
| `Gun()`                                  | `gundb.New()`                                       |
| `Gun(['http://host:8765/gun'])`          | `gundb.New(gundb.Options{Peers: []string{url}})`    |
| `Gun({web: server})`                     | `http.Handle("/gun", db)`                           |
| `gun.get('a').get('b')`                  | `db.Get("a").Get("b")`                              |
| `.put({name: 'x'})`                      | `.Put(ctx, User{Name: "x"})`                        |
| `.put(data, ack => ...)`                 | `err := .PutAck(ctx, data)`                         |
| `.once(cb)`                              | `v, err := .Once[T](ctx)`                           |
| `.on(cb)` / `.off()`                     | `off := .On(func(v T) {...})` / `off()`             |
| `.set(item)`                             | `itemRef, err := .Set(ctx, item)`                   |
| `.map().on(cb)`                          | `.Map().On(func(key string, v T) {...})`            |
| `.put(null)`                             | `.Put(ctx, nil)`                                    |
| `.get('a').put(gun.get('b'))`            | `.Get("a").Put(ctx, db.Get("b"))`                   |

Rules worth knowing:

- Data is a graph of nodes. `db.Get(key)` is a node; `.Get(field)` goes one level down.
- `Put` merges field by field (it never replaces a whole node). Conflicts resolve with GUN's HAM, so every peer ends up with the same value.
- Arrays are not supported (same as GUN): use `Set`.
- `Once` returns `gundb.ErrNotFound` when there is nothing there.
- `On` callbacks run on their own goroutine; a deleted value arrives as the zero value (`nil` for pointer types).

## 1. Store and read data

A DB with no options is a local database. `Put` structs, maps or single values; `Once[T]` reads them back into any type.

<!-- example: examples/basic/main.go -->
```go
// Put and read data. No server needed: a DB on its own is a local database.
package main

import (
	"context"
	"fmt"
	"log"

	"ella.to/gundb"
)

type User struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func main() {
	ctx := context.Background()
	db := gundb.New()
	defer db.Close()

	alice := db.Get("users").Get("alice")
	check(alice.Put(ctx, User{Name: "Alice", Age: 30}))
	check(alice.Get("age").Put(ctx, 31)) // update a single field

	user, err := alice.Once[User](ctx)
	check(err)
	fmt.Println(user.Name, user.Age) // Alice 31

	name, err := alice.Get("name").Once[string](ctx)
	check(err)
	fmt.Println(name) // Alice

	_, err = db.Get("users").Get("bob").Once[User](ctx)
	fmt.Println(err) // gundb: not found
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
```

## 2. Live updates

`On` calls you now and on every change, local or remote. Type inference picks `T` from your callback.

<!-- example: examples/realtime/main.go -->
```go
// Subscribe with On: the callback runs now and on every change.
package main

import (
	"context"
	"fmt"
	"time"

	"ella.to/gundb"
)

func main() {
	ctx := context.Background()
	db := gundb.New()
	defer db.Close()

	topic := db.Get("room").Get("topic")

	off := topic.On(func(t string) {
		fmt.Println("topic is now:", t)
	})
	defer off()

	topic.Put(ctx, "Hello")
	time.Sleep(50 * time.Millisecond)
	topic.Put(ctx, "Go + GUN")
	time.Sleep(50 * time.Millisecond)
}
```

## 3. Nested data and links

Nested structs become their own linked nodes (souls like `mark/pet`, same as GUN JS). Put a `*Ref` to link existing nodes.

<!-- example: examples/nested/main.go -->
```go
// Nested structs become linked nodes, and you can follow links by path.
package main

import (
	"context"
	"fmt"
	"log"

	"ella.to/gundb"
)

type Pet struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type Person struct {
	Name string  `json:"name"`
	Pet  *Pet    `json:"pet,omitempty"`
	Boss *Person `json:"boss,omitempty"`
}

func main() {
	ctx := context.Background()
	db := gundb.New()
	defer db.Close()

	// The pet is stored as its own node ("mark/pet"), linked from mark.
	check(db.Get("mark").Put(ctx, Person{Name: "Mark", Pet: &Pet{Name: "Fluffy", Kind: "cat"}}))

	pet, err := db.Get("mark").Get("pet").Once[Pet](ctx)
	check(err)
	fmt.Println(pet.Name, "is a", pet.Kind) // Fluffy is a cat

	// Link two existing nodes by putting a ref.
	check(db.Get("amy").Put(ctx, Person{Name: "Amy"}))
	check(db.Get("mark").Get("boss").Put(ctx, db.Get("amy")))

	// Once loads linked nodes into nested structs.
	mark, err := db.Get("mark").Once[Person](ctx)
	check(err)
	fmt.Println(mark.Name, "works for", mark.Boss.Name) // Mark works for Amy

	// Writing through a link changes the linked node itself.
	check(db.Get("mark").Get("boss").Get("name").Put(ctx, "Amy B."))
	amy, err := db.Get("amy").Once[Person](ctx)
	check(err)
	fmt.Println(amy.Name) // Amy B.
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
```

## 4. Lists

`Set` adds to an unordered list, `Map().On` watches every item, `Put(ctx, nil)` deletes.

<!-- example: examples/lists/main.go -->
```go
// Lists: Set adds items, Map().On watches them, Put(nil) removes one.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"ella.to/gundb"
)

type Todo struct {
	Title string `json:"title"`
	Done  bool   `json:"done"`
}

func main() {
	ctx := context.Background()
	db := gundb.New()
	defer db.Close()

	todos := db.Get("todos")

	// A pointer type gets nil when an item is removed.
	off := todos.Map().On(func(id string, t *Todo) {
		if t == nil {
			fmt.Println("removed", id)
			return
		}
		fmt.Printf("%-10s done=%v\n", t.Title, t.Done)
	})
	defer off()

	milk, err := todos.Set(ctx, Todo{Title: "Buy milk"})
	check(err)
	_, err = todos.Set(ctx, Todo{Title: "Learn Go"})
	check(err)
	time.Sleep(50 * time.Millisecond)

	check(milk.Get("done").Put(ctx, true)) // update an item
	time.Sleep(50 * time.Millisecond)

	check(todos.Get(milk.Key()).Put(ctx, nil)) // remove it from the list
	time.Sleep(50 * time.Millisecond)

	all, err := todos.Once[map[string]Todo](ctx)
	check(err)
	fmt.Println(len(all), "item left")
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
```

## 5. Sync between peers

Give `Options.Peers` a URL and the DB syncs in real time. A `*DB` is an `http.Handler`, so any DB can be the relay.

<!-- example: examples/sync/main.go -->
```go
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
```

## 6. Run a relay

This is all a server needs. JS clients connect with `Gun(['http://localhost:8765/gun'])`.

<!-- example: examples/relay/main.go -->
```go
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
```

## 7. Browser + Go chat

A Go bot and browsers running GUN.js share one chat room. Open http://localhost:8765 in two tabs. The page is [`examples/chat/index.html`](examples/chat/index.html).

<!-- example: examples/chat/main.go -->
```go
// A chat room shared by browsers (GUN.js) and a Go bot, through a Go relay.
//
//	go run ./examples/chat
//
// then open http://localhost:8765 in two tabs.
package main

import (
	"context"
	_ "embed"
	"log"
	"net/http"
	"strings"
	"time"

	"ella.to/gundb"
)

//go:embed index.html
var page []byte

type Message struct {
	Who  string `json:"who"`
	Text string `json:"text"`
	At   int64  `json:"at"`
}

func main() {
	db := gundb.New()
	defer db.Close()

	// The bot answers every new message that isn't its own.
	start := time.Now().UnixMilli()
	chat := db.Get("chat")
	chat.Map().On(func(_ string, m Message) {
		if m.Who == "bot" || m.At < start {
			return
		}
		reply := Message{Who: "bot", Text: strings.ToUpper(m.Text) + "!", At: time.Now().UnixMilli()}
		if _, err := chat.Set(context.Background(), reply); err != nil {
			log.Print(err)
		}
	})

	http.Handle("/gun", db)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.Write(page) })
	log.Print("chat on http://localhost:8765")
	log.Fatal(http.ListenAndServe(":8765", nil))
}
```

## 8. Persist to disk

A `Store` is two methods. This one keeps each node in a JSON file; run it twice and the counter keeps going.

<!-- example: examples/persist/main.go -->
```go
// A custom Store: keep every node in its own JSON file. Run it twice.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"

	"ella.to/gundb"
)

// FileStore implements gundb.Store with one file per node.
type FileStore struct{ Dir string }

func (s FileStore) path(soul string) string {
	return filepath.Join(s.Dir, url.PathEscape(soul)+".json")
}

func (s FileStore) Get(_ context.Context, soul string) (*gundb.Node, error) {
	b, err := os.ReadFile(s.path(soul))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // unknown soul
	}
	if err != nil {
		return nil, err
	}
	n := new(gundb.Node)
	return n, json.Unmarshal(b, n)
}

func (s FileStore) Put(_ context.Context, n *gundb.Node) error {
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	tmp := s.path(n.Soul) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(n.Soul))
}

func main() {
	ctx := context.Background()
	dir := filepath.Join(os.TempDir(), "gundb-persist-example")
	check(os.MkdirAll(dir, 0o755))

	db := gundb.New(gundb.Options{Store: FileStore{Dir: dir}})
	defer db.Close()

	runs, err := db.Get("stats").Get("runs").Once[int](ctx)
	if err != nil && !errors.Is(err, gundb.ErrNotFound) {
		log.Fatal(err)
	}
	runs++
	check(db.Get("stats").Get("runs").Put(ctx, runs))
	fmt.Printf("run #%d (data in %s)\n", runs, dir)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
```

## 9. A CLI for any GUN peer

Works against Go and JS relays alike: `go run ./examples/client put users.alice '{"name":"Alice"}'`.

<!-- example: examples/client/main.go -->
```go
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
```

## Bring your own transport

WebSocket is built in. For anything else, implement `transport.Conn` (Send, Recv, Close, RemoteAddr)
and hand connections to the DB:

```go
go db.Attach(conn)                       // one connection
go db.Serve(ctx, listener)               // a transport.Listener
db := gundb.New(gundb.Options{Dialer: d, Peers: []string{addr}}) // dial with your transport.Dialer
```
