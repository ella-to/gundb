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
| `.once(cb)`, links left as `{'#': soul}` | `n, err := .Node(ctx)` / `links, err := .Links(ctx)` |
| `.on(cb)` / `.off()`                     | `off := .On(func(v T) {...})` / `off()`             |
| `.set(item)`                             | `itemRef, err := .Set(ctx, item)`                   |
| `.map().on(cb)`                          | `.Map().On(func(key string, v T) {...})`            |
| `.put(null)`                             | `.Put(ctx, nil)`                                    |
| `.get('a').put(gun.get('b'))`            | `.Get("a").Put(ctx, db.Get("b"))`                   |
| `.val(cb)` (old name of `.once`)         | `v, err := .Once[T](ctx)`                           |
| `.key('k')` (removed from GUN.js)        | `db.Get("k").Put(ctx, data)`, then link to `db.Get("k")` |
| `get: {'#': {'*': 'chat/'}}` (radisk)    | `souls, err := db.Souls(ctx, gundb.Lex{Prefix: "chat/"})` |

Rules worth knowing:

- Data is a graph of nodes. `db.Get(key)` is a node; `.Get(field)` goes one level down.
- `Put` merges field by field (it never replaces a whole node). Conflicts resolve with GUN's HAM, so every peer ends up with the same value.
- Arrays are not supported (same as GUN): use `Set`.
- `Once` returns `gundb.ErrNotFound` when there is nothing there.
- `On` first delivers the value as it is when you call it, then every change after. Callbacks run on their own goroutine, and changes that land faster than your callback runs may arrive merged into one call (you always get the latest value). A deleted value arrives as the zero value (`nil` for pointer types).

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

### Circular links

Two nodes can link to each other, like users who are each other's friend.
You can't build that as a Go pointer cycle and `Put` it: `encoding/json`
rejects cycles. Put each node under its own key, then link them with refs.

<!-- example: examples/circular/main.go -->
```go
// Circular links: two users who are each other's friend, and choosing
// your own key for a node.
package main

import (
	"context"
	"fmt"
	"log"

	"ella.to/gundb"
)

type User struct {
	Name   string `json:"name"`
	Friend *User  `json:"friend,omitempty"`
}

func main() {
	ctx := context.Background()
	db := gundb.New()
	defer db.Close()

	// A Go pointer cycle can't be Put: encoding/json refuses it.
	alice := &User{Name: "Alice"}
	alice.Friend = &User{Name: "Bob", Friend: alice}
	fmt.Println(db.Get("alice").Put(ctx, alice) != nil) // true

	// Instead, Put each user under its own key, then link them with refs.
	// The key you pass to db.Get is the node's soul.
	check(db.Get("alice").Put(ctx, User{Name: "Alice"}))
	check(db.Get("bob").Put(ctx, User{Name: "Bob"}))
	check(db.Get("alice").Get("friend").Put(ctx, db.Get("bob")))
	check(db.Get("bob").Get("friend").Put(ctx, db.Get("alice")))

	// Paths follow links as far as you like.
	name, err := db.Get("alice").Get("friend").Get("friend").Get("friend").Get("name").Once[string](ctx)
	check(err)
	fmt.Println(name) // Bob

	// Once stops where the cycle closes: the link back to alice comes back
	// as {"#": "alice"}, which decodes into an empty User.
	a, err := db.Get("alice").Once[User](ctx)
	check(err)
	fmt.Printf("%s -> %s -> %q\n", a.Name, a.Friend.Name, a.Friend.Friend.Name) // Alice -> Bob -> ""

	raw, err := db.Get("alice").Once[map[string]any](ctx)
	check(err)
	fmt.Println(raw["friend"].(map[string]any)["friend"]) // map[#:alice]

	// Writing through a link changes the linked node, from either side.
	check(db.Get("bob").Get("friend").Get("name").Put(ctx, "Alice A."))
	name, err = db.Get("alice").Get("name").Once[string](ctx)
	check(err)
	fmt.Println(name) // Alice A.

	// Your own key for a list item: Put the node under the key you want,
	// then add a ref to it with Set. The item's key in the list is its soul.
	carol, err := db.Get("people").Set(ctx, db.Get("my-own-key"))
	check(err)
	check(db.Get("my-own-key").Put(ctx, User{Name: "Carol"}))
	fmt.Println(carol.Key()) // my-own-key
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
```

Things to know:

- `Once` follows links into nested structs but stops at a node it is already
  inside. The link that closes the cycle comes back as `{"#": soul}`, so
  `a.Friend.Friend` above is an empty `User`. To keep going, read by path
  (`.Get("friend").Get("friend")...`) or start a new `Once` from the other node.
- Don't `Put` a struct you read back from a cycle. Its empty back-link would
  write the zero values (here `name: ""`) over the real node.
- There is no `.val()` or `.key()`. In GUN.js `.val()` was renamed
  `.once()`, which is `Once[T]` here. GUN.js removed `.key()`. Here
  `Ref.Key()` only *returns* the last key of a path. To choose a node's key
  yourself, put it at `db.Get("your-key")`, and link to it from wherever you
  need it (with `Put(ctx, db.Get("your-key"))` or `Set(ctx, db.Get("your-key"))`).

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

### A list of links: friends

A list can hold links to nodes that live elsewhere, like a user's friends.
`Set(ctx, db.Get("bob"))` adds a link (not a copy), keyed by Bob's soul, so
updating Bob shows up in every list that links to him, and removing him from
one list leaves his node alone.

<!-- example: examples/friends/main.go -->
```go
// A list of links: each user has a set of friends that point at other
// user nodes. Add, update, remove and watch them.
package main

import (
	"context"
	"fmt"
	"log"
	"maps"
	"slices"
	"time"

	"ella.to/gundb"
)

type User struct {
	Name string `json:"name"`
	City string `json:"city"`
}

func main() {
	ctx := context.Background()
	db := gundb.New()
	defer db.Close()

	// Every user is a node under its own key.
	for key, u := range map[string]User{
		"alice": {Name: "Alice", City: "Paris"},
		"bob":   {Name: "Bob", City: "Berlin"},
		"carol": {Name: "Carol", City: "Rome"},
	} {
		check(db.Get(key).Put(ctx, u))
	}
	friends := db.Get("alice").Get("friends")

	// Watch the list. The key is the friend's soul; a removed friend
	// arrives as nil.
	off := friends.Map().On(func(key string, u *User) {
		if u == nil {
			fmt.Println("removed", key)
			return
		}
		fmt.Printf("friend %s: %s from %s\n", key, u.Name, u.City)
	})
	defer off()

	// Add: Set a ref to an existing user. This stores a link, not a copy.
	_, err := friends.Set(ctx, db.Get("bob"))
	check(err)
	_, err = friends.Set(ctx, db.Get("carol"))
	check(err)
	time.Sleep(50 * time.Millisecond)

	// Friendship both ways is just another link back.
	_, err = db.Get("bob").Get("friends").Set(ctx, db.Get("alice"))
	check(err)

	// Update: change the user node. Every list that links to it sees it.
	check(db.Get("bob").Get("city").Put(ctx, "Madrid"))
	time.Sleep(50 * time.Millisecond)

	// Remove: delete the link by its key. Bob's node itself is untouched.
	check(friends.Get("bob").Put(ctx, nil))
	time.Sleep(50 * time.Millisecond)

	// Read the whole list once. Removed friends are left out.
	all, err := friends.Once[map[string]User](ctx)
	check(err)
	fmt.Println(slices.Sorted(maps.Keys(all))) // [carol]

	bob, err := db.Get("bob").Once[User](ctx)
	check(err)
	fmt.Println(bob.Name, "still exists, in", bob.City) // Bob still exists, in Madrid

	// Add the same friend again: the link comes back.
	_, err = friends.Set(ctx, db.Get("bob"))
	check(err)
	time.Sleep(50 * time.Millisecond)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
```

| To...                     | Do                                                   |
| ------------------------- | ---------------------------------------------------- |
| add a friend              | `friends.Set(ctx, db.Get("bob"))`                    |
| update a friend           | `db.Get("bob").Get("city").Put(ctx, "Madrid")` (or through the list: `friends.Get("bob").Get("city").Put(...)`) |
| remove a friend           | `friends.Get("bob").Put(ctx, nil)`                   |
| read all friends          | `friends.Once[map[string]User](ctx)`                 |
| watch the list            | `friends.Map().On(func(key string, u *User) {...})`  |
| list friends, unloaded    | `friends.Links(ctx)` (see [Large lists](#large-lists-read-the-links-load-what-you-need)) |

Things to know:

- Setting the same ref twice doesn't duplicate it: the key is the soul, so
  a list never holds a friend twice. Removing and adding again brings the
  link back.
- To read the friends as part of the user, give the struct a map field:
  ``Friends map[string]User `json:"friends,omitempty"` ``. Don't `Put` that
  struct back after reading it, though: it would write each friend's fields
  too (see [Circular links](#circular-links)). Change lists with `Set` and
  `Put(nil)` instead.
- A removed item stays in the node as a tombstone (`null`), like in GUN.js,
  so every peer learns about the removal. `Once` and `Map().On` skip it.

### Large lists: read the links, load what you need

`Once` and `Map().On` on a list load every item. Each item is its own node, so
reading a list of 1,000 asks peers for 1,001 nodes, sends all of them over the
network and keeps all of them in the local store, even if you only show 10.
The cost grows with the size of the list, not with what you use.

`Links` reads only the list node and returns its links, key -> soul. That one
read is enough to:

- **count** the items (`len(links)`),
- **page** through them, loading only the page you show,
- **check membership** (`_, ok := links[soul]`),
- **fetch only what's new**: compare against the souls you already have and
  load the rest.

The example shows the newest 10 messages of a feed of 1,000, both ways:

<!-- example: examples/feed/main.go -->
```go
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
```

```
Once:  loaded 1000 messages in 22ms
Links: 1000 messages in the feed, newest 10 loaded in 4ms
  message 1000
  message 999
  ...
```

Both clients sit next to the relay on the same machine. Over a real network
the gap is wider: the `Once` client pays for every message's bytes and every
peer round trip, the `Links` client for one node plus 10 messages.

Things to know:

- **Order.** Souls made by `Set`, in Go and in GUN.js, start with the time
  they were made, so sorting them sorts the items oldest to newest. A Go
  peer keeps that order even for items it makes in the same millisecond;
  GUN.js only roughly. Across peers the time comes from each writer's
  clock, so a device with a wrong clock sorts wrong. If exact order
  matters, choose the keys yourself, for example
  `feed.Get(key).Put(ctx, db.Get(soul))` with a sortable key, and sort the
  keys `Links` returns instead of the souls.
- **One read, not live.** `Links` reads once. To follow a list as it
  changes, use `Map().On`, which does load every item.
- **Small lists don't need it.** If you show every item anyway, `Once` is
  simpler and costs the same.
- `Node` is the lower-level read under `Links`: the node exactly as stored,
  with plain values, links as `Link{Soul}`, deleted fields as `Null` and
  every field's state. Use it for nodes that mix values and links, or to
  see what was deleted.

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

## 6. Work offline, sync later

A DB is a complete database on its own, so reads and writes keep working without a connection. Peers in `Options.Peers` are re-dialled every 2 seconds. When the connection is back, writes made in the meantime are sent, and the DB asks again for everything it has read, so changes made elsewhere arrive too. Run this one to watch a relay go down and come back while two devices keep editing the same note.

<!-- example: examples/offline/main.go -->
```go
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
```

The `wait` helpers only make the output above predictable. In an app you rarely wait: write with `Put`, and show data with `On`, which runs again whenever a change arrives, before or after a reconnect.

### How offline changes merge

- **Different fields merge.** HAM resolves every field on its own, so the phone's `body` and the laptop's `status` both survive.
- **The same field: the later write wins**, by the writer's clock at the time of the write, not by who reconnects first. A tie goes to the larger value. Every peer applies the same rule, so all of them end up with the same note.
- **Deletes are writes too.** `Put(ctx, nil)` while offline is queued like any other write and wins or loses by the same rule.

### Things to know

- **`Put` while offline succeeds**: the write is applied locally and queued. `PutAck` returns `gundb.ErrNoPeers` when nobody is connected, but the write is still saved and queued.
- **The queue lives in memory**, up to 10,000 writes per peer. If the program exits before it reconnects, its offline writes are still in its store, and they are sent once the restarted program reads those nodes: every time a DB asks peers for a node (the first read, and again on each reconnect), it also offers them its own copy. Peers keep only what is newer than what they have.
- **Keep offline data across restarts** with a store that persists: [`pebblestore`](#11-persist-to-disk) on disk, or [`idbstore`](#15-run-in-the-browser-webassembly) in a browser. The default store is in memory, so a restart starts empty.
- **Clocks matter.** HAM compares the writers' clocks. A device whose clock is far ahead wins conflicts, and other peers hold its writes back until their own clock catches up.

## 7. Run a relay

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

## 8. Permissions: who can read and write

A relay decides what each connected user may do. Three `Options` set it up:

| Option | Called | Decides |
| ------ | ------ | ------- |
| `Authenticate(r *http.Request) (user, customID, error)` | once per connection, on the WebSocket upgrade | who is connecting: read a token, cookie or header; `""` is anonymous, an error refuses the connection (401). `customID` is your own label for the connection (e.g. a device ID), reported in `PeerInfo` |
| `CanWrite(user, soul, field) bool` | for every field of every incoming put | whether `user` may write it; one denied field rejects the whole put, and the writer's `PutAck` returns `gundb.ErrForbidden` |
| `CanRead(user, soul) bool` | for every get, and for every node the relay sends out | whether `user` may see the node; denied nodes are never sent, so `Once` returns `gundb.ErrNotFound` and `On` never fires |

Rules see souls and fields, not chain paths, so it helps to know how a path is stored. This write

```go
db.Get("users").Get("ali").Get("messages").Get("m1").Put(ctx, Message{Text: "hi"})
```

touches these nodes:

| soul | field | value |
| ---- | ----- | ----- |
| `users` | `ali` | link to `users/ali` |
| `users/ali` | `messages` | link to `users/ali/messages` |
| `users/ali/messages` | `m1` | link to `users/ali/messages/m1` |
| `users/ali/messages/m1` | `text` | `"hi"` |

So a write rule that checks `soul + "/" + field` sees the path `users/ali/...` at every level, including the `users` → `ali` link, which stops Bob from pointing Ali's name at a node of his own. The example gives every user write access to their own `users/<name>/...`, read access to everything except `users/<name>/private`, and lets anonymous users read only.

<!-- example: examples/auth/main.go -->
```go
// Read and write permissions on a relay. Everyone can read
// users/<name>/messages, only <name> can write it, and users/<name>/private
// is for <name>'s eyes only.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"

	"ella.to/gundb"
)

type Message struct {
	Text string `json:"text"`
}

// tokens stands in for your real login: sessions, JWTs, API keys...
var tokens = map[string]string{"tok-ali": "ali", "tok-bob": "bob"}

// owner returns who a path belongs to: "users/ali/messages/m1" -> "ali".
func owner(path string) string {
	rest, ok := strings.CutPrefix(path, "users/")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, "/")
	return name
}

func main() {
	ctx := context.Background()

	relay := gundb.New(gundb.Options{
		// Who is connecting? Browsers cannot set WebSocket headers, so the
		// token comes in the URL. No token: an anonymous, read-only user.
		Authenticate: func(r *http.Request) (string, string, error) {
			token := r.URL.Query().Get("token")
			if token == "" {
				return "", "", nil
			}
			if user, ok := tokens[token]; ok {
				return user, "", nil
			}
			return "", "", errors.New("unknown token")
		},
		// Write: users/<name>/... belongs to <name>. The check is on
		// soul + "/" + field, so the link users -> ali is protected too.
		CanWrite: func(user, soul, field string) bool {
			if o := owner(soul + "/" + field); o != "" {
				return o == user
			}
			return user != "" // anything else: any logged-in user
		},
		// Read: everything, except users/<name>/private.
		CanRead: func(user, soul string) bool {
			if o := owner(soul); o != "" && strings.HasPrefix(soul, "users/"+o+"/private") {
				return o == user
			}
			return true
		},
	})
	defer relay.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	go http.Serve(l, relay)
	url := "ws://" + l.Addr().String() + "/gun"

	ali := gundb.New(gundb.Options{Peers: []string{url + "?token=tok-ali"}})
	bob := gundb.New(gundb.Options{Peers: []string{url + "?token=tok-bob"}})
	defer ali.Close()
	defer bob.Close()

	// Ali writes her own messages. Use keyed Puts, not Set, for protected
	// data: m1 gets the soul users/ali/messages/m1, which the rules can
	// match; Set would give it a random soul.
	messages := ali.Get("users").Get("ali").Get("messages")
	check(messages.Get("m1").PutAck(ctx, Message{Text: "Hello from Ali"}))
	check(ali.Get("users").Get("ali").Get("private").Get("note").PutAck(ctx, "buy a gift for Bob"))

	// Bob can read them...
	m, err := bob.Get("users").Get("ali").Get("messages").Get("m1").Once[Message](ctx)
	check(err)
	fmt.Println("bob reads:", m.Text) // bob reads: Hello from Ali

	// ...but not write them. PutAck reports the relay's answer.
	err = bob.Get("users").Get("ali").Get("messages").Get("m1").PutAck(ctx, Message{Text: "hacked"})
	fmt.Println("bob writes:", errors.Is(err, gundb.ErrForbidden)) // bob writes: true

	// And Ali's private node is never sent to him.
	_, err = bob.Get("users").Get("ali").Get("private").Get("note").Once[string](ctx)
	fmt.Println("bob reads the private note:", err) // gundb: not found
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
```

### Logging in from a client

The token travels with the WebSocket upgrade request.

- **Browsers** cannot set headers on a WebSocket, so put the token in the URL, `Gun(['https://example.com/gun?token=' + token])`, or rely on a cookie: same-site cookies are sent with the upgrade, so `Authenticate` can read `r.Cookie("session")`.
- **Go clients** can use the URL the same way, or send a header through the dialer:

```go
db := gundb.New(gundb.Options{
	Peers: []string{"wss://example.com/gun"},
	Dialer: ws.Dialer{Options: &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	}},
})
```

- **Other transports**: identify the user yourself and hand the connection over with `db.AttachAs(conn, user, customID)`.

### Example: a chat with user names and passwords

The whole flow end to end. The client sends a user name and password to a normal HTTP endpoint and gets a token back. It connects to the relay with that token, and from then on the relay knows who it is.

1. **Log in.** `POST /login` checks the password and returns a random token, remembered in `sessions`.
2. **Connect.** The client dials `/gun?token=<token>`. `Authenticate` looks the token up; an unknown token refuses the connection, so only logged-in users get in at all.
3. **Post.** `CanWrite` lets each user write only under `chat/<their name>/...`, so nobody can post as someone else. Reading is open to every logged-in user.

<!-- example: examples/login/main.go -->
```go
// A chat where everyone logs in with a user name and password. The server
// hands out a token, the client connects with it, and the relay lets each
// user post only under their own name.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"ella.to/gundb"
)

type Message struct {
	Text string `json:"text"`
}

// passwords stands in for your user database. Store hashes (bcrypt,
// argon2), never plain passwords.
var passwords = map[string]string{"ali": "ali-secret", "bob": "bob-secret"}

// sessions maps a token to the user it was issued to.
var (
	mu       sync.Mutex
	sessions = map[string]string{}
)

// login checks a user name and password and returns a new token.
//
//	POST /login  user=ali&password=ali-secret  ->  200 <token>
func login(w http.ResponseWriter, r *http.Request) {
	user, password := r.FormValue("user"), r.FormValue("password")
	want, ok := passwords[user]
	if !ok || subtle.ConstantTimeCompare([]byte(password), []byte(want)) != 1 {
		http.Error(w, "wrong user or password", http.StatusUnauthorized)
		return
	}
	token := rand.Text()
	mu.Lock()
	sessions[token] = user
	mu.Unlock()
	io.WriteString(w, token)
}

func main() {
	ctx := context.Background()

	relay := gundb.New(gundb.Options{
		// Authentication: who is connecting? The token comes in the URL
		// (browsers cannot set WebSocket headers). No valid token, no
		// connection: only logged-in users can read or post.
		Authenticate: func(r *http.Request) (string, string, error) {
			mu.Lock()
			user, ok := sessions[r.URL.Query().Get("token")]
			mu.Unlock()
			if !ok {
				return "", "", errors.New("not logged in")
			}
			return user, "", nil
		},
		// Authorization: what may they do? Messages live at
		// chat/<user>/<id>, and only <user> may write there.
		CanWrite: func(user, soul, field string) bool {
			path := strings.Split(soul+"/"+field, "/") // chat, ali, m1, text
			return path[0] == "chat" && path[1] == user
		},
	})
	defer relay.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", login)
	mux.Handle("/gun", relay)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	go http.Serve(l, mux)
	server := "http://" + l.Addr().String()

	// A wrong password gets no token.
	_, err = connect(server, "bob", "guess")
	fmt.Println("bob, wrong password:", err) // 401 Unauthorized

	ali, err := connect(server, "ali", "ali-secret")
	check(err)
	defer ali.Close()
	bob, err := connect(server, "bob", "bob-secret")
	check(err)
	defer bob.Close()

	// Ali posts under her own name...
	check(ali.Get("chat").Get("ali").Get("m1").PutAck(ctx, Message{Text: "hi, I'm Ali"}))

	// ...and Bob, logged in too, reads it.
	m, err := bob.Get("chat").Get("ali").Get("m1").Once[Message](ctx)
	check(err)
	fmt.Println("bob reads:", m.Text) // bob reads: hi, I'm Ali

	// Bob cannot post as Ali.
	err = bob.Get("chat").Get("ali").Get("m2").PutAck(ctx, Message{Text: "I'm Ali, honest"})
	fmt.Println("bob posts as ali:", errors.Is(err, gundb.ErrForbidden)) // true
}

// connect logs in and returns a DB connected to the relay with the token.
func connect(server, user, password string) (*gundb.DB, error) {
	resp, err := http.PostForm(server+"/login", url.Values{"user": {user}, "password": {password}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}
	token, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	ws := "ws" + strings.TrimPrefix(server, "http") + "/gun?token=" + url.QueryEscape(string(token))
	return gundb.New(gundb.Options{Peers: []string{ws}}), nil
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
```

A browser does the same with `fetch`, then hands the token to GUN.js:

```js
const res = await fetch('/login', { method: 'POST', body: new URLSearchParams({ user, password }) });
if (!res.ok) throw new Error('wrong user or password');
const token = await res.text();
const gun = Gun([location.origin + '/gun?token=' + encodeURIComponent(token)]);
gun.get('chat').get(user).get(Date.now().toString()).put({ text: 'hello' });
```

To log out, delete the token from `sessions`. The open connection stays authenticated until it closes, because `Authenticate` runs once per connection. In a real app, also give tokens an expiry, or use a signed cookie or JWT instead of the in-memory map.

### Things to know

- **The relay enforces the rules.** They apply to peers that connect to it. The relay's own code (`relay.Get(...).Put`) and the peers it dials itself (`Options.Peers`) are trusted. If you run several relays, give each one the same rules.
- **Use keyed `Put` for protected data, not `Set`.** `Set` gives each item a random soul, which a path rule cannot tell apart from anyone else's. `messages.Get(id).Put(ctx, msg)` gives it `users/ali/messages/<id>`.
- **Check `PutAck`.** Like in GUN, a write is applied locally first and then sent. If the relay rejects it, nobody else ever sees it, but the writer's own copy keeps it until it is overwritten. `Put` does not wait for the answer, `PutAck` does.
- **Reads are per node.** A user who may read `users/ali` sees its `private` field, but that field is only a link: the node it points to, with the content, is never sent. Put private data in its own node, as the example does.
- **Clients offer what they read.** When a DB asks a peer for a node, it also sends its own copy, so writes made offline get through (see [section 6](#6-work-offline-sync-later)). For a node the client may only read, the relay rejects that copy like any other forbidden write, and the client ignores the error.
- **Every message runs your rules.** Keep them fast and in memory: look the session up once in `Authenticate`, not on every `CanRead`.
- **Protect the token.** Use `wss://` (TLS) in production. Tokens in URLs can end up in proxy logs; short-lived tokens or cookies limit the damage.
- **Rules or SEA?** Rules are enforced by a relay you trust and can also hide data. [SEA](#9-users-and-encryption-sea) is enforced by every peer with signatures, so nobody has to trust a server, but signed data is public unless you encrypt it. They combine: SEA for "only Ali writes her space", rules for what anonymous users may see.

### Who is online

`Options.OnConnect` and `Options.OnDisconnect` are called for every connection, with a `PeerInfo`:

| Field | What it is |
|---|---|
| `PeerID` | unique to this connection, chosen by gundb; a reconnect gets a new one |
| `User`, `CustomID` | what `Authenticate` returned |
| `URL` | on a client: which of its `Peers` connected |
| `Remote` | the remote address |

On a client, they tell you when you are online. On a relay, every tab is its own connection, so a user is online while they have at least one. To tell tabs from devices, have the client send a device ID (kept in `localStorage`, which every tab of a browser shares) and return it as the custom ID:

```go
Authenticate: func(r *http.Request) (string, string, error) {
	user, err := lookUp(r.URL.Query().Get("token"))
	return user, r.URL.Query().Get("device"), err
},
OnConnect:    func(p gundb.PeerInfo) { /* p.User, p.CustomID, p.PeerID */ },
OnDisconnect: func(p gundb.PeerInfo) { /* same PeerInfo as its OnConnect */ },
```

[examples/presence](examples/presence/main.go) counts tabs per user and device. Things to know:

- **The hooks run on the connection's goroutine.** Return quickly; hand slow work to another goroutine.
- **A vanished peer is noticed within about 40s.** A closed tab or a stopped client is reported straight away. A phone that loses Wi-Fi closes nothing, so gundb pings every peer every 20s and drops one that does not answer within 20s. Browsers and Node answer pings on their own.
- **The custom ID comes from the client.** It labels connections; it proves nothing. Base permissions on the user only.

## 9. Users and encryption (SEA)

SEA is GUN's security layer. A user is a key pair; the part of the graph they own, their *space*, is the node `~<pub>` and every soul under it (`~<pub>/profile`, ...). Everything written there is signed with the user's key, and **every peer checks every signature**, so nobody can write into someone else's space, and no server has to be trusted to enforce it. Accounts are stored exactly like `gun.user()` stores them: a user created in Go can log in from GUN.js with the same alias and password, and the other way around.

<!-- example: examples/sea/main.go -->
```go
// SEA users: signed data only its owner can write, and encrypted messages
// only the recipient can read. Compatible with gun.user() in GUN.js.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"

	"ella.to/gundb"
	"ella.to/gundb/sea"
)

type Profile struct {
	Name string `json:"name"`
}

func main() {
	ctx := context.Background()

	// A relay and three devices. No rules to configure: every peer checks
	// SEA signatures on every write.
	relay := gundb.New()
	defer relay.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	go http.Serve(l, relay)
	url := "ws://" + l.Addr().String() + "/gun"
	phone := gundb.New(gundb.Options{Peers: []string{url}})  // ali's
	laptop := gundb.New(gundb.Options{Peers: []string{url}}) // ali's
	bobs := gundb.New(gundb.Options{Peers: []string{url}})   // bob's
	defer phone.Close()
	defer laptop.Close()
	defer bobs.Close()

	// Sign up on the phone. Writes through ali's refs are signed by her key.
	ali, err := phone.CreateUser(ctx, "ali", "correct horse battery")
	check(err)
	check(ali.Get("profile").PutAck(ctx, Profile{Name: "Ali"}))

	// Anyone can read her space, by public key.
	p, err := laptop.User(ali.Pub()).Get("profile").Once[Profile](ctx)
	check(err)
	fmt.Println("profile:", p.Name) // profile: Ali

	// Nobody else can write it: unsigned writes are rejected everywhere.
	err = laptop.User(ali.Pub()).Get("profile").Get("name").Put(ctx, "Mallory")
	fmt.Println("forged write:", errors.Is(err, gundb.ErrUnverified)) // forged write: true

	// Log in on the laptop with the password: same keys, same space.
	ali, err = laptop.Login(ctx, "ali", "correct horse battery")
	check(err)
	check(ali.Get("profile").Get("name").PutAck(ctx, "Ali N."))

	// Private messages: encrypt with a secret only ali and bob can derive.
	bob, err := bobs.CreateUser(ctx, "bob", "staple battery horse")
	check(err)
	bobEPub, err := laptop.User(bob.Pub()).Get("epub").Once[string](ctx)
	check(err)
	secret, err := sea.Secret(bobEPub, ali.Pair())
	check(err)
	enc, err := sea.Encrypt("meet at 6", secret)
	check(err)
	check(ali.Get("to-bob").PutAck(ctx, enc)) // public, but unreadable

	// Bob, on his device, derives the same secret from ali's public epub.
	stored, err := bobs.User(ali.Pub()).Get("to-bob").Once[string](ctx)
	check(err)
	aliEPub, err := bobs.User(ali.Pub()).Get("epub").Once[string](ctx)
	check(err)
	shared, err := sea.Secret(aliEPub, bob.Pair())
	check(err)
	plain, err := sea.Decrypt(stored, shared)
	check(err)
	var msg string
	check(json.Unmarshal(plain, &msg))
	fmt.Println("bob reads:", msg) // bob reads: meet at 6
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
```

| gundb | GUN.js |
| ----- | ------ |
| `user, err := db.CreateUser(ctx, alias, pass)` | `gun.user().create(alias, pass)` |
| `user, err := db.Login(ctx, alias, pass)` | `gun.user().auth(alias, pass)` |
| `user := db.LoginPair(pair)` | `gun.user().auth(pair)` |
| `user.Get("profile").Put(ctx, p)` | `gun.user().get('profile').put(p)` |
| `db.User(pub).Get("profile")` | `gun.user(pub).get('profile')` |
| `sea.NewPair`, `Sign`, `Verify`, `Encrypt`, `Decrypt`, `Secret`, `Work` | `SEA.pair`, `sign`, `verify`, `encrypt`, `decrypt`, `secret`, `work` |

### How it works

- **Signed values.** In a user's space every value is stored as `{":":value,"~":signature}`. The signature covers the soul, the field, the value and its HAM state, so it cannot be moved to another field or replayed as a newer write. Reads (`Once`, `On`, `Map`) give you the plain value.
- **Every write is checked**, local or from a peer, as GUN.js peers do: unsigned or wrongly signed data in a user's space fails with `gundb.ErrUnverified` and is never stored or relayed. `~@alias` entries must point at themselves, and souls containing `#` hold content whose SHA-256 is its key.
- **Accounts.** `~@alice` lists the public keys that claim the alias. `~<pub>` holds `pub`, the signed `alias` and `epub`, and `auth`: the private keys, encrypted with a key derived from the password (PBKDF2, 100,000 rounds).

### Things to know

- **Signed is not secret.** Anyone can read a user's space. Encrypt what is private: `sea.Encrypt(data, user.Pair().EPriv)` for yourself, or with `sea.Secret(theirEPub, user.Pair())` for someone else, as the example does.
- **Aliases are not unique**, as in GUN: anyone can add their key to `~@alice`, and `Login` tries each key with the password. Identify people by `Pub()`, not by alias.
- **A lost password is a lost account.** The keys can only be unlocked with it. To sign in without one, keep `user.Pair()` somewhere safe and use `db.LoginPair`.
- **Outside users' spaces nothing changes**: anyone can write, unless your relay has [rules](#8-permissions-who-can-read-and-write).
- **Not supported:** SEA certificates (`SEA.certify`, which GUN.js marks experimental) and signatures from before 2020. Writes that use them are rejected.

### Encrypted group chat with a shared key

A group only its members can read, with the building blocks above. Two rules
shape the layout:

- **Each member writes to their own space.** Only alice can write `~alice/...`
  (SEA certificates, which would let others write there, aren't supported), so
  each member keeps their messages in their own feed, and readers merge the
  feeds. That costs nothing: every message is signed by its author for free.
- **The owner signs the member list.** It lives in the owner's space, so nobody
  else can add themselves to it.

The owner makes a random group key and stores one copy per member, encrypted
with `sea.Secret` between the owner and that member. Removing someone means a
new key *version*, given only to the members left.

<!-- example: examples/group/main.go -->
```go
// Encrypted group chat with a shared group key. Only members can read it,
// and removing a member locks them out of what is said next.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"ella.to/gundb"
	"ella.to/gundb/sea"
)

// Message is what a member stores in their own feed: ciphertext, and the
// version of the group key it was encrypted with.
type Message struct {
	Version int    `json:"version"`
	Data    string `json:"data"`
}

// group is "~owner/groups/<id>" in the graph:
//
//	members/<pub>          true for each member (signed by the owner)
//	version                the current key version
//	keys/<version>/<pub>   the group key, encrypted for that member
//
// and each member writes their messages to "~member/groups/<id>/messages".
type group struct {
	db    *gundb.DB
	owner string // the owner's public key
	id    string
}

func main() {
	ctx := context.Background()
	// One DB stands in for the network: in a real app each person runs
	// their own peer, and only their own *gundb.User (their keys).
	db := gundb.New()
	defer db.Close()

	alice, bob, carol := newUser(ctx, db, "alice"), newUser(ctx, db, "bob"), newUser(ctx, db, "carol")
	eve := newUser(ctx, db, "eve") // never a member

	// Alice creates the group with bob and carol.
	g := &group{db: db, owner: alice.Pub(), id: "team"}
	check(g.setMembers(ctx, alice, alice, bob, carol))

	check(g.send(ctx, alice, "hi team"))
	check(g.send(ctx, bob, "hello!"))
	g.show(ctx, "carol", carol)

	// Everything is public in the graph, but eve has no key to read it.
	g.show(ctx, "eve", eve)

	// Alice removes carol: a new key version, given only to who is left.
	check(g.setMembers(ctx, alice, alice, bob))
	check(g.send(ctx, alice, "carol is gone"))
	g.show(ctx, "bob", bob)
	g.show(ctx, "carol", carol) // she still has the old key, not the new one
}

// setMembers makes members the group (owner included) and hands each of
// them a new group key, encrypted with a secret only the owner and that
// member can derive.
func (g *group) setMembers(ctx context.Context, owner *gundb.User, members ...*gundb.User) error {
	ref := owner.Get("groups").Get(g.id)
	version, _ := ref.Get("version").Once[int](ctx) // 0 for a new group
	version++

	key := make([]byte, 32)
	rand.Read(key)
	list := map[string]any{}
	old, _ := ref.Get("members").Once[map[string]bool](ctx)
	for pub := range old {
		list[pub] = nil // removed unless added back below
	}
	for _, m := range members {
		secret, err := sea.Secret(m.Pair().EPub, owner.Pair())
		if err != nil {
			return err
		}
		enc, err := sea.Encrypt(base64.StdEncoding.EncodeToString(key), secret)
		if err != nil {
			return err
		}
		if err := ref.Get("keys").Get(strconv.Itoa(version)).Get(m.Pub()).Put(ctx, enc); err != nil {
			return err
		}
		list[m.Pub()] = true
	}
	if err := ref.Get("members").Put(ctx, list); err != nil {
		return err
	}
	return ref.Get("version").Put(ctx, version) // last: members find their key first
}

// key returns the group key of version v for user u, decrypted with the
// secret u shares with the owner.
func (g *group) key(ctx context.Context, u *gundb.User, v int) (string, error) {
	enc, err := g.db.User(g.owner).Get("groups").Get(g.id).Get("keys").Get(strconv.Itoa(v)).Get(u.Pub()).Once[string](ctx)
	if err != nil {
		return "", err // no key for u: not a member at version v
	}
	ownerEPub, err := g.db.User(g.owner).Get("epub").Once[string](ctx)
	if err != nil {
		return "", err
	}
	secret, err := sea.Secret(ownerEPub, u.Pair())
	if err != nil {
		return "", err
	}
	raw, err := sea.Decrypt(enc, secret)
	if err != nil {
		return "", err
	}
	var key string
	return key, json.Unmarshal(raw, &key)
}

// send encrypts text with the current group key and adds it to u's feed.
func (g *group) send(ctx context.Context, u *gundb.User, text string) error {
	v, err := g.db.User(g.owner).Get("groups").Get(g.id).Get("version").Once[int](ctx)
	if err != nil {
		return err
	}
	key, err := g.key(ctx, u, v)
	if err != nil {
		return fmt.Errorf("%s is not a member: %w", u.Alias(), err)
	}
	enc, err := sea.Encrypt(text, key)
	if err != nil {
		return err
	}
	// A time-ordered key sorts the feed; the random part keeps keys unique.
	id := fmt.Sprintf("%020d-%s", time.Now().UnixNano(), rand.Text()[:6])
	return u.Get("groups").Get(g.id).Get("messages").Get(id).Put(ctx, Message{Version: v, Data: enc})
}

// show prints the conversation as u sees it: every member's feed, merged in
// time order, decrypted where u has the key.
func (g *group) show(ctx context.Context, name string, u *gundb.User) {
	fmt.Printf("--- %s sees:\n", name)
	members, _ := g.db.User(g.owner).Get("groups").Get(g.id).Get("members").Once[map[string]bool](ctx)
	type line struct{ id, from, text string }
	var lines []line
	keys := map[int]string{}
	for _, pub := range slices.Sorted(maps.Keys(members)) {
		from, _ := g.db.User(pub).Get("alias").Once[string](ctx)
		feed, _ := g.db.User(pub).Get("groups").Get(g.id).Get("messages").Once[map[string]Message](ctx)
		for id, m := range feed {
			text := "[can't read]"
			if _, ok := keys[m.Version]; !ok {
				keys[m.Version], _ = g.key(ctx, u, m.Version)
			}
			if raw, err := sea.Decrypt(m.Data, keys[m.Version]); err == nil {
				json.Unmarshal(raw, &text)
			}
			lines = append(lines, line{id, from, text})
		}
	}
	slices.SortFunc(lines, func(a, b line) int { return strings.Compare(a.id, b.id) })
	for _, l := range lines {
		fmt.Printf("%s: %s\n", l.from, l.text)
	}
}

func newUser(ctx context.Context, db *gundb.DB, alias string) *gundb.User {
	u, err := db.CreateUser(ctx, alias, alias+" password")
	check(err)
	return u
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
```

Output:

```
--- carol sees:
alice: hi team
bob: hello!
--- eve sees:
alice: [can't read]
bob: [can't read]
--- bob sees:
alice: hi team
bob: hello!
alice: carol is gone
--- carol sees:
alice: hi team
bob: hello!
alice: [can't read]
```

What a shared key does not give you:

- **Removed members keep the past.** Carol still has key version 1, and the
  ciphertext is copied to every peer. You can't take back what was already
  readable.
- **A leaked key is a leaked history.** Everything encrypted under that version
  can be read with it. There is no forward secrecy. For that, use sender keys
  (below).
- **New members can read the history** of the version they're given. Start a
  new version when someone joins if they shouldn't.

### Encrypted group chat with sender keys (a ratchet)

Package `sea/senderkey` implements Signal's *sender keys*, the scheme Signal
and WhatsApp use for groups. Every member has their own **chain**:

- **Every message gets a new key.** The key comes from the chain with HMAC and
  HKDF. The chain then steps forward, and the old chain key is deleted. Keys
  only go forward, so a device's current state can't decrypt anything it
  already read (**forward secrecy**).
- **Each member shares their chain** with every other member, sealed to that
  member's **prekey** with `senderkey.Seal`. A prekey is a key pair that stays
  on the device; its public half is published in the member's space. Sealing
  uses a one-off key pair and SEA ECDH, so even the member's SEA account keys
  (their password) can't open it, and once the prekey is deleted, nothing can.
- **A shared chain starts where the sender is now**, so a new member reads from
  the moment they joined, never the history.
- **Removing a member means everyone left starts a new chain**, which the
  removed member never gets.

Sender keys fit GUN well:

- **No two writers share state.** Each chain belongs to one sender and lives
  in that sender's space. Messages carry a counter, so they can arrive late,
  out of order or after time offline (`Receiver` keeps keys it skipped, up to
  `senderkey.MaxSkip`). There is no need for the single global order that
  MLS-style protocols require.
- **Authenticity is already there.** A message read from `~bob` was signed by
  bob and checked by every peer, so the ratchet doesn't need to sign anything.
- **The ciphertext is plain SEA.** Each message is `sea.Encrypt(text,
  messageKey)`, and sealed chains are SEA ECDH plus `sea.Encrypt`, so a GUN.js
  client can join the group by implementing the chain step with WebCrypto.

<!-- example: examples/senderkeys/main.go -->
```go
// Encrypted group chat with sender keys (a ratchet): every message has its
// own key, new members can't read history, removed members can't read what
// comes next, and a stolen device can't read what it already read.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"ella.to/gundb"
	"ella.to/gundb/sea"
	"ella.to/gundb/sea/senderkey"
)

// In the graph (all public, all signed by whoever owns the space):
//
//	~owner/groups/<id>/members/<pub>          true for each member
//	~member/prekey                            the member's current prekey (an epub)
//	~member/groups/<id>/chain                 the ID of the member's current chain
//	~member/groups/<id>/keys/<chain>/<pub>    member's chain, sealed for <pub>
//	~member/groups/<id>/messages/<msgID>      a senderkey.Message
//
// On each device, never in the graph: prekeys, chains, and read messages.
type member struct {
	*gundb.User
	name    string
	db      *gundb.DB
	prekeys map[string]*sea.Pair           // by epub
	sender  *senderkey.Sender              // my chain
	chains  map[string]*senderkey.Receiver // other members' chains, by ID
	seen    map[string]bool                // message IDs already handled
}

type group struct {
	owner string // the owner's public key
	id    string
}

func main() {
	ctx := context.Background()
	// One DB stands in for the network: in a real app each person runs
	// their own peer, and each member value is their own device.
	db := gundb.New()
	defer db.Close()
	alice, bob, carol, dave := join(ctx, db, "alice"), join(ctx, db, "bob"), join(ctx, db, "carol"), join(ctx, db, "dave")

	// Alice creates the group. Each member starts a chain and shares it.
	g := group{owner: alice.Pub(), id: "team"}
	check(alice.setMembers(ctx, g, alice, bob, carol))
	for _, m := range []*member{alice, bob, carol} {
		check(m.newChain(ctx, g))
	}
	check(alice.send(ctx, g, "hi team"))
	check(bob.send(ctx, g, "hello!"))
	carol.read(ctx, g)

	// Dave joins. The others share their chains as they are now, so dave
	// reads from here on but not the history: chains only move forward.
	check(alice.setMembers(ctx, g, alice, bob, carol, dave))
	check(dave.newChain(ctx, g))
	for _, m := range []*member{alice, bob, carol} {
		check(m.share(ctx, g, m.sender, dave.Pub()))
	}
	check(alice.send(ctx, g, "welcome dave"))
	dave.read(ctx, g)

	// Carol leaves. Everyone left starts a new chain that carol never gets.
	check(alice.setMembers(ctx, g, alice, bob, dave))
	for _, m := range []*member{alice, bob, dave} {
		check(m.newChain(ctx, g))
	}
	check(bob.send(ctx, g, "carol can't read this"))
	carol.read(ctx, g)
	bob.read(ctx, g)

	// read opened every new chain sealed for bob, so he can now replace
	// his prekey and delete the old one. He still reads what comes next.
	check(bob.newPrekey(ctx))
	check(alice.send(ctx, g, "still private"))
	bob.read(ctx, g)

	// A thief with bob's password and a copy of his device can't read
	// anything bob already read: his chains have moved past those
	// messages, and the sealed keys in the graph need the deleted prekey.
	thief := bob.stolen("thief with bob's device")
	thief.read(ctx, g)

	// Bob's password alone (his SEA keys, on a new device) opens nothing:
	// chains are sealed to prekeys, which never leave the device.
	login, err := db.Login(ctx, "bob", "bob password")
	check(err)
	fresh := &member{User: login, name: "bob's password on a new device", db: db, prekeys: map[string]*sea.Pair{}, chains: map[string]*senderkey.Receiver{}, seen: map[string]bool{}}
	fresh.read(ctx, g)
}

// join creates a user and publishes their first prekey.
func join(ctx context.Context, db *gundb.DB, alias string) *member {
	u, err := db.CreateUser(ctx, alias, alias+" password")
	check(err)
	m := &member{User: u, name: alias, db: db, prekeys: map[string]*sea.Pair{}, chains: map[string]*senderkey.Receiver{}, seen: map[string]bool{}}
	check(m.newPrekey(ctx))
	return m
}

// newPrekey publishes a new prekey for others to seal chains to, and
// deletes the old ones. Do it once the chains sealed to them are open.
func (m *member) newPrekey(ctx context.Context) error {
	p, err := sea.NewPair()
	if err != nil {
		return err
	}
	clear(m.prekeys)
	m.prekeys[p.EPub] = p
	return m.Get("prekey").Put(ctx, p.EPub)
}

// setMembers replaces the member list (only the owner can write it).
func (m *member) setMembers(ctx context.Context, g group, members ...*member) error {
	ref := m.Get("groups").Get(g.id).Get("members")
	list := map[string]any{}
	old, _ := ref.Once[map[string]bool](ctx)
	for pub := range old {
		list[pub] = nil
	}
	for _, x := range members {
		list[x.Pub()] = true
	}
	return ref.Put(ctx, list)
}

func (g group) members(ctx context.Context, db *gundb.DB) []string {
	list, _ := db.User(g.owner).Get("groups").Get(g.id).Get("members").Once[map[string]bool](ctx)
	return slices.Sorted(maps.Keys(list))
}

// newChain starts a new chain and shares it with every other member.
func (m *member) newChain(ctx context.Context, g group) error {
	s, err := senderkey.NewSender()
	if err != nil {
		return err
	}
	m.sender = s
	if err := m.Get("groups").Get(g.id).Get("chain").Put(ctx, s.Key.Chain); err != nil {
		return err
	}
	var others []string
	for _, pub := range g.members(ctx, m.db) {
		if pub != m.Pub() {
			others = append(others, pub)
		}
	}
	return m.share(ctx, g, s, others...)
}

// share seals s, as it is now, to the current prekey of each member in to.
func (m *member) share(ctx context.Context, g group, s *senderkey.Sender, to ...string) error {
	for _, pub := range to {
		prekey, err := m.db.User(pub).Get("prekey").Once[string](ctx)
		if err != nil {
			return err
		}
		sealed, err := senderkey.Seal(s.Share(), prekey)
		if err != nil {
			return err
		}
		if err := m.Get("groups").Get(g.id).Get("keys").Get(s.Key.Chain).Get(pub).Put(ctx, sealed); err != nil {
			return err
		}
	}
	return nil
}

// send encrypts text with the next key of m's chain and adds it to m's feed.
func (m *member) send(ctx context.Context, g group, text string) error {
	msg, err := m.sender.Encrypt(text)
	if err != nil {
		return err
	}
	id := fmt.Sprintf("%020d-%s", time.Now().UnixNano(), rand.Text()[:6])
	m.seen[id] = true
	return m.Get("groups").Get(g.id).Get("messages").Get(id).Put(ctx, msg)
}

// read opens the other members' new chains, then prints the messages m
// hasn't seen yet from their feeds, in time order. Each message can be
// decrypted only once, so a real app stores the plain text on the device.
func (m *member) read(ctx context.Context, g group) {
	type line struct{ id, text string }
	var lines []line
	for _, pub := range g.members(ctx, m.db) {
		if pub == m.Pub() {
			continue
		}
		if chain, err := m.db.User(pub).Get("groups").Get(g.id).Get("chain").Once[string](ctx); err == nil && m.chains[chain] == nil {
			m.open(ctx, g, pub, chain) // open it now, while we have the prekey
		}
		from, _ := m.db.User(pub).Get("alias").Once[string](ctx)
		feed, _ := m.db.User(pub).Get("groups").Get(g.id).Get("messages").Once[map[string]senderkey.Message](ctx)
		for _, id := range slices.Sorted(maps.Keys(feed)) {
			if m.seen[id] {
				continue
			}
			m.seen[id] = true
			lines = append(lines, line{id, from + ": " + m.decrypt(ctx, g, pub, feed[id])})
		}
	}
	slices.SortFunc(lines, func(a, b line) int { return strings.Compare(a.id, b.id) })
	fmt.Printf("--- %s reads:\n", m.name)
	for _, l := range lines {
		fmt.Println(l.text)
	}
}

// open finds from's chain sealed for m and opens it with m's prekey.
func (m *member) open(ctx context.Context, g group, from, chain string) error {
	sealed, err := m.db.User(from).Get("groups").Get(g.id).Get("keys").Get(chain).Get(m.Pub()).Once[senderkey.Sealed](ctx)
	if err != nil {
		return errors.New("no key for me")
	}
	prekey, ok := m.prekeys[sealed.PreKey]
	if !ok {
		return errors.New("sealed to a prekey I don't have")
	}
	key, err := senderkey.Open(sealed, prekey)
	if err != nil {
		return err
	}
	m.chains[chain] = senderkey.NewReceiver(key)
	return nil
}

func (m *member) decrypt(ctx context.Context, g group, from string, msg senderkey.Message) string {
	r, ok := m.chains[msg.Chain]
	if !ok {
		if err := m.open(ctx, g, from, msg.Chain); err != nil {
			return "[can't read: " + err.Error() + "]"
		}
		r = m.chains[msg.Chain]
	}
	raw, err := r.Decrypt(msg)
	if errors.Is(err, senderkey.ErrOld) {
		return "[can't read: before my key, or already read]"
	} else if err != nil {
		return "[can't read: " + err.Error() + "]"
	}
	var text string
	json.Unmarshal(raw, &text)
	return text
}

// stolen returns a copy of m's device as it is saved now: its prekeys and
// chains (round-tripped through JSON, as a device would store them), with
// no record of which messages it has read.
func (m *member) stolen(name string) *member {
	saved, err := json.Marshal(struct {
		Prekeys map[string]*sea.Pair
		Chains  map[string]*senderkey.Receiver
	}{m.prekeys, m.chains})
	check(err)
	t := &member{User: m.User, name: name, db: m.db, seen: map[string]bool{}}
	check(json.Unmarshal(saved, &struct {
		Prekeys *map[string]*sea.Pair
		Chains  *map[string]*senderkey.Receiver
	}{&t.prekeys, &t.chains}))
	return t
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
```

Output:

```
--- carol reads:
alice: hi team
bob: hello!
--- dave reads:
alice: [can't read: before my key, or already read]
bob: [can't read: before my key, or already read]
alice: welcome dave
--- carol reads:
alice: welcome dave
bob: [can't read: no key for me]
--- bob reads:
alice: hi team
alice: welcome dave
--- bob reads:
alice: still private
--- thief with bob's device reads:
alice: [can't read: before my key, or already read]
alice: [can't read: before my key, or already read]
alice: [can't read: before my key, or already read]
--- bob's password on a new device reads:
alice: [can't read: sealed to a prekey I don't have]
alice: [can't read: sealed to a prekey I don't have]
alice: [can't read: sealed to a prekey I don't have]
```

The API:

| Call | What it does |
| ---- | ------------ |
| `senderkey.NewSender()` | start your chain |
| `sender.Encrypt(data)` | encrypt with the next message key, then step the chain |
| `sender.Share()` | your chain as it is now, to give to a member |
| `senderkey.Seal(key, prekeyEPub)` / `Open(sealed, prekey)` | send a chain to one member's prekey, and open it there |
| `senderkey.NewReceiver(key)` / `receiver.Decrypt(msg)` | read another member's chain |

Things to know:

- **Keep the state on the device, never in the graph.** That means `Sender`,
  each `Receiver` and the prekeys. They marshal to JSON for local storage:
  a file, or IndexedDB in the browser. Putting them in the graph, even
  encrypted to yourself, gives up forward secrecy.
- **A message opens once.** Its key is deleted after `Decrypt`, so store the
  plain text locally if you need it again (a second `Decrypt` returns
  `senderkey.ErrOld`).
- **Open new chains as soon as they appear**, as `read` does, then rotate your
  prekey with `newPrekey`. A chain sealed to a prekey you've already deleted
  can't be opened. Rotate prekeys regularly: until a prekey is deleted, whoever
  steals it can open the chains sealed to it.
- **A stolen device reads what comes next**, until the members start new
  chains. Sender keys don't heal on their own. Start new chains on a schedule
  and whenever a device may be compromised, as you do when someone leaves.
- **Every device is a member.** State can't be synced between devices without
  giving up forward secrecy, so give each device its own user (or at least its
  own prekey and chain) in the member list. A new device doesn't see history,
  as the last part of the example shows.
- **The graph shows who talks when.** Member lists, feed sizes and timing are
  public. Only the content is hidden.
- **Watch feeds with `Map().On` in a real client.** The examples read with
  `Once` to keep the output simple. Over the network, `Once` on a node you
  already read returns your local copy, which can be a moment behind the
  relay. `Map().On` delivers each message as it arrives.
- **Check who you're talking to.** Member lists hold public keys, and aliases
  aren't unique. Compare keys out of band (a safety number or QR code) before
  you trust a member.

## 10. Browser + Go chat

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

## 11. Persist to disk

By default a DB keeps its data in memory. To keep it on disk, pass a different `Store` in `Options`; nothing else in your code changes. `storage/pebblestore` uses [Pebble](https://github.com/cockroachdb/pebble), an LSM-tree engine: writes are appends to a log and reads are short scans over sorted keys, so it stays fast with large datasets. Run this twice and the counter keeps going.

<!-- example: examples/pebble/main.go -->
```go
// Swap the storage: keep the data on disk with Pebble. Run it twice.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"ella.to/gundb"
	"ella.to/gundb/storage/pebblestore"
)

func main() {
	ctx := context.Background()
	dir := filepath.Join(os.TempDir(), "gundb-pebble-example")

	store, err := pebblestore.Open(dir)
	check(err)
	defer store.Close() // deferred first, so it closes after the DB

	db := gundb.New(gundb.Options{Store: store})
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

| Store | Where data lives | History | Use it for |
| ----- | ---------------- | ------- | ---------- |
| `gundb.NewMemoryStore()` (default) | memory | no | tests, relays that don't need to keep data |
| `storage/memstore` | memory | no | inspecting what a peer holds (`Souls()`) |
| `storage/pebblestore` | disk | yes | anything you want to keep |
| `storage/idbstore` | browser IndexedDB | no | Go compiled to WebAssembly ([section 15](#15-run-in-the-browser-webassembly)) |

Close the store after the DB (defer it first). By default every write is synced to disk before it is accepted; `pebblestore.Options{NoSync: true}` is much faster, but a crash can lose the last few writes.

## 12. Read history

`pebblestore` keeps every write that won HAM, deletions (`null`) included. The DB always reads the latest version; for older ones, ask the store:

- `store.History(ctx, soul, field)` yields every version of a field, newest first. Each `Version` has the `State` (a timestamp) and the `Value`.
- `store.GetAt(ctx, soul, t)` returns the whole node as it was at state `t`: for each field, the newest version written at or before `t`.

States are GUN's HAM states: milliseconds since the Unix epoch, as a `float64`. Convert with `time.UnixMilli(int64(v.State))` and `float64(t.UnixMilli())`.

<!-- example: examples/history/main.go -->
```go
// Read old versions: pebblestore keeps every accepted write.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"ella.to/gundb"
	"ella.to/gundb/storage/pebblestore"
)

type Doc struct {
	Title  string `json:"title"`
	Status string `json:"status"`
}

func main() {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "gundb-history-example")
	check(err)
	defer os.RemoveAll(dir)

	store, err := pebblestore.Open(dir)
	check(err)
	defer store.Close()
	db := gundb.New(gundb.Options{Store: store})
	defer db.Close()

	doc := db.Get("readme") // a root node: its soul is "readme"
	check(doc.Put(ctx, Doc{Title: "Draft", Status: "writing"}))
	time.Sleep(10 * time.Millisecond)
	checkpoint := time.Now() // remember a moment in time
	time.Sleep(10 * time.Millisecond)
	check(doc.Get("title").Put(ctx, "Final"))
	check(doc.Get("status").Put(ctx, "published"))

	// The DB always reads the latest version.
	now, err := doc.Once[Doc](ctx)
	check(err)
	fmt.Println("now:", now.Title, now.Status) // now: Final published

	// History reads the store directly, by soul and field, newest first.
	for v, err := range store.History(ctx, "readme", "title") {
		check(err)
		at := time.UnixMilli(int64(v.State)).Format("15:04:05.000")
		fmt.Println("title was", v.Value, "at", at)
	}

	// GetAt returns the whole node as it was at a moment. States are
	// milliseconds since the Unix epoch.
	old, err := store.GetAt(ctx, "readme", float64(checkpoint.UnixMilli()))
	check(err)
	fmt.Println("at checkpoint:", old.Fields["title"], old.Fields["status"]) // Draft writing
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
```

History is addressed by soul. For `db.Get(key)` the soul is `key`. A nested node such as `db.Get("users").Get("alice")` gets the soul `users/alice` when a `Put` creates it, unless the field already links to another node, as it does after a `Set` or after putting a `*Ref`. Values in `History` and `GetAt` are `gundb.Value`s: `gundb.String`, `gundb.Number`, `gundb.Bool`, `gundb.Null` (deleted) or `gundb.Link` (a reference to another node).

History is kept only by peers whose store is a `pebblestore`, and it is never pruned.

## 13. Write your own Store

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

The runtime does all HAM merging and calls `Put` with the whole merged node. A store that also implements `gundb.FieldStore` gets `PutFields` instead, with only the fields that changed. That is how `pebblestore` writes history without rewriting unchanged fields. A `FieldStore` that also implements `gundb.FieldReader` (`GetFields(ctx, soul, fields)`) is asked for only the fields a merge touches, so writing one field of a node with thousands of fields stays as cheap as writing a small one; the default store, `memstore` and `pebblestore` do. A `FieldStore` that implements `gundb.BatchStore` (`PutBatch(ctx, nodes)`) gets every node of a multi-node write (a `Set`, a nested struct) in one call, so it can commit them with one disk sync; `pebblestore` does. A store that implements `gundb.SoulStore` (`ListSouls(ctx, prefix)`) can also answer LEX queries over souls (`db.Souls`, or `{"#": {"*": prefix}}` from GUN.js). The default store, `memstore` and `pebblestore` all do.

## 14. A CLI for any GUN peer

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

## 15. Run in the browser (WebAssembly)

The `gundb` package compiles to WebAssembly (`GOOS=js GOARCH=wasm`), so the same Go code runs in a browser tab. It connects to peers over the browser's WebSocket, and [`storage/idbstore`](storage/idbstore) keeps its data in IndexedDB, so the app keeps working offline and survives reloads and browser restarts.

This example is a notes app that syncs between browsers:

```sh
go run ./examples/browser   # builds app/ to wasm, serves it and a relay
```

Open http://localhost:8766 in two browsers (or a normal and a private window: tabs of one browser share their IndexedDB, so they always look in sync). Add notes in both. Then stop the server and add a note in one of them. Reload it, or quit and reopen the browser: the page still loads and the notes are still there. Start the server again, and the other browser receives the note written offline.

The browser half, compiled to wasm:

<!-- example: examples/browser/app/main.go -->
```go
//go:build js && wasm

// The browser half of examples/browser: Go compiled to WebAssembly, with its
// data in IndexedDB. Notes survive reloads, can be written offline, and sync
// with every other tab through the relay.
package main

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"syscall/js"
	"time"

	"ella.to/gundb"
	"ella.to/gundb/storage/idbstore"
)

type Note struct {
	Text string `json:"text"`
	At   int64  `json:"at"`
}

var document = js.Global().Get("document")

func main() {
	ctx := context.Background()
	store, err := idbstore.Open(ctx, "gundb-example")
	if err != nil {
		setText("status", err.Error())
		return
	}
	origin := js.Global().Get("location").Get("origin").String()
	db := gundb.New(gundb.Options{Store: store, Peers: []string{origin + "/gun"}})

	// Show every note, oldest first. Local ones appear at once, even offline.
	notes := db.Get("notes")
	var mu sync.Mutex
	seen := map[string]Note{}
	notes.Map().On(func(id string, n Note) {
		mu.Lock()
		defer mu.Unlock()
		seen[id] = n
		render(seen)
	})

	// Add a note on submit. Never block inside a js.Func: use a goroutine.
	input := document.Call("getElementById", "text")
	onSubmit := js.FuncOf(func(_ js.Value, args []js.Value) any {
		args[0].Call("preventDefault")
		text := input.Get("value").String()
		input.Set("value", "")
		if text != "" {
			go notes.Set(ctx, Note{Text: text, At: time.Now().UnixMilli()})
		}
		return nil
	})
	document.Call("getElementById", "form").Call("addEventListener", "submit", onSubmit)

	for {
		if n := len(db.Peers()); n > 0 {
			setText("status", "online: syncing with the relay")
		} else {
			setText("status", "offline: notes are saved in IndexedDB and sync when the relay is back")
		}
		time.Sleep(time.Second)
	}
}

func render(seen map[string]Note) {
	ids := slices.SortedFunc(maps.Keys(seen), func(a, b string) int { return cmp.Compare(seen[a].At, seen[b].At) })
	list := document.Call("getElementById", "notes")
	list.Set("textContent", "")
	for _, id := range ids {
		li := document.Call("createElement", "li")
		li.Set("textContent", seen[id].Text)
		list.Call("append", li)
	}
	setText("count", fmt.Sprintf("%d notes", len(ids)))
}

func setText(id, text string) { document.Call("getElementById", id).Set("textContent", text) }
```

The rest of [`examples/browser`](examples/browser) is plumbing every Go wasm app needs:

- [`main.go`](examples/browser/main.go) builds the app with `GOOS=js GOARCH=wasm go build`, then serves `app.wasm`, Go's loader `$(go env GOROOT)/lib/wasm/wasm_exec.js` and the page. It also runs the relay at `/gun`.
- [`index.html`](examples/browser/index.html) loads `wasm_exec.js` and starts `app.wasm`.
- [`sw.js`](examples/browser/sw.js) is a service worker that caches those files, so the page opens while the server is down. Without it the app still keeps its data, but the browser can't load the page to show it.

### Things to know

- **Never block inside a `js.Func`.** IndexedDB, like WebSocket, answers through browser callbacks, so `Put`, `Once`, `On` (which reads the current value before it returns) and the store wait for the browser. Called directly from an event handler, they wait for a callback that can't run until the handler returns, and the page deadlocks. Start a goroutine (`go notes.Set(...)`), as the submit handler above does. `On` callbacks themselves already run on their own goroutine.
- **One database per origin and name.** `idbstore.Open(ctx, name)` opens the IndexedDB database `name`. Every tab of your site that opens the same name shares the data. Each node is one record (soul → its GUN wire JSON) in the object store `nodes`, readable in the browser's developer tools under Application → IndexedDB.
- **Browsers may evict storage.** Under storage pressure a browser can clear a site's data unless it was granted persistent storage. Ask for it with `navigator.storage.persist()` if the data matters. Some private-browsing modes have no IndexedDB; `Open` returns an error there.
- **Only the client side works in a browser.** `ServeHTTP`, `ws.Listen` and `pebblestore` need a real server. `idbstore` builds only for `js/wasm`.
- **Size.** A Go wasm app is several MB (about 2 MB gzipped for this one with `-ldflags="-s -w"`). Serve it compressed.
- **Testing.** `go test ./storage/idbstore` runs the store's tests in Node with an IndexedDB emulation, after `(cd storage/idbstore/testdata && npm install)`.

## Bring your own transport

WebSocket is built in. For anything else, implement `transport.Conn` (Send, Recv, Close, RemoteAddr)
and hand connections to the DB:

```go
go db.Attach(conn)                       // one connection
go db.Serve(ctx, listener)               // a transport.Listener
db := gundb.New(gundb.Options{Dialer: d, Peers: []string{addr}}) // dial with your transport.Dialer
```
