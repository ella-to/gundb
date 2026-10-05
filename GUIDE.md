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

## 7. Permissions: who can read and write

A relay decides what each connected user may do. Three `Options` set it up:

| Option | Called | Decides |
| ------ | ------ | ------- |
| `Authenticate(r *http.Request) (user, error)` | once per connection, on the WebSocket upgrade | who is connecting: read a token, cookie or header; `""` is anonymous, an error refuses the connection (401) |
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
		Authenticate: func(r *http.Request) (string, error) {
			token := r.URL.Query().Get("token")
			if token == "" {
				return "", nil
			}
			if user, ok := tokens[token]; ok {
				return user, nil
			}
			return "", errors.New("unknown token")
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

- **Other transports**: identify the user yourself and hand the connection over with `db.AttachAs(conn, user)`.

### Things to know

- **The relay enforces the rules.** They apply to peers that connect to it. The relay's own code (`relay.Get(...).Put`) and the peers it dials itself (`Options.Peers`) are trusted. If you run several relays, give each one the same rules.
- **Use keyed `Put` for protected data, not `Set`.** `Set` gives each item a random soul, which a path rule cannot tell apart from anyone else's. `messages.Get(id).Put(ctx, msg)` gives it `users/ali/messages/<id>`.
- **Check `PutAck`.** Like in GUN, a write is applied locally first and then sent. If the relay rejects it, nobody else ever sees it, but the writer's own copy keeps it until it is overwritten. `Put` does not wait for the answer, `PutAck` does.
- **Reads are per node.** A user who may read `users/ali` sees its `private` field, but that field is only a link: the node it points to, with the content, is never sent. Put private data in its own node, as the example does.
- **Every message runs your rules.** Keep them fast and in memory: look the session up once in `Authenticate`, not on every `CanRead`.
- **Protect the token.** Use `wss://` (TLS) in production. Tokens in URLs can end up in proxy logs; short-lived tokens or cookies limit the damage.
- **Rules or SEA?** Rules are enforced by a relay you trust and can also hide data. [SEA](#8-users-and-encryption-sea) is enforced by every peer with signatures, so nobody has to trust a server, but signed data is public unless you encrypt it. They combine: SEA for "only Ali writes her space", rules for what anonymous users may see.

## 8. Users and encryption (SEA)

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
- **Outside users' spaces nothing changes**: anyone can write, unless your relay has [rules](#7-permissions-who-can-read-and-write).
- **Not supported:** SEA certificates (`SEA.certify`, which GUN.js marks experimental) and signatures from before 2020. Writes that use them are rejected.

## 9. Browser + Go chat

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

## 10. Persist to disk

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

Close the store after the DB (defer it first). By default every write is synced to disk before it is accepted; `pebblestore.Options{NoSync: true}` is much faster, but a crash can lose the last few writes.

## 11. Read history

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

## 12. Write your own Store

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

The runtime does all HAM merging and calls `Put` with the whole merged node. A store that also implements `gundb.FieldStore` gets `PutFields` instead, with only the fields that changed. That is how `pebblestore` writes history without rewriting unchanged fields.

## 13. A CLI for any GUN peer

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
