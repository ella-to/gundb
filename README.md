# gundb

[GUN](https://github.com/amark/gun) for Go: a realtime, offline-first graph
database that syncs peer to peer. It speaks the same wire protocol as GUN.js,
so Go and JavaScript peers (browser or Node) share data directly.

```go
db := gundb.New(gundb.Options{Peers: []string{"ws://localhost:8765/gun"}})

alice := db.Get("users").Get("alice")
alice.Put(ctx, User{Name: "Alice", Age: 30})

user, err := alice.Once[User](ctx)                  // read once
off := alice.On(func(u User) { fmt.Println(u.Name) }) // live updates
```

A relay server is one line, and browsers can connect to it with `Gun(['http://localhost:8765/gun'])`:

```go
http.Handle("/gun", gundb.New())
```

**→ Start with the [guide](GUIDE.md).** Every snippet in it is a runnable
program in [`examples/`](examples/).

## What's inside

- **The JS chain API, typed.** `Get`, `Put`, `Once[T]`, `On`, `Set` and `Map`
  are built on Go 1.27 generic methods. Structs go in and come out; nested
  structs become linked nodes.
- **Wire-compatible with GUN.js.** This covers the DAM handshake and dedup,
  HAM conflict resolution (including JS's UTF-16 string order for
  tie-breaks), GUN's number formatting, batched and chunked messages, and
  LEX queries over keys and souls (`db.Souls`). The [interop tests](interop/) run real GUN.js peers
  against Go peers in both directions.
- **Relays.** Any DB relays like a GUN.js server (AXE-style). Puts go to
  the peers that asked for those souls. Gets that miss locally are forwarded,
  and acks are routed back to whoever asked.
- **SEA users.** `CreateUser` / `Login` like `gun.user()`, signed user
  spaces that every peer verifies, and package `sea` for signing and
  encryption, byte-compatible with GUN.js. Package `sea/senderkey` adds
  encrypted group chat with Signal-style sender keys.
- **Permissions.** A relay is an `http.Handler`: authenticate with your
  own middleware, and `CanRead` / `CanWrite` decide, per field, what each
  connection may read and write from the session in its context, for Go
  and GUN.js clients alike.
- **Quotas.** `Options.Quota` counts the nodes and bytes each owner (by
  default, each SEA user) keeps, and refuses writes past their plan's
  limits, like free at 1,000 nodes and 10 MB, pro at 10,000 and 100 MB.
- **One-way peers.** `?gundb_mode=read` in a peer's URL only reads from it (a
  read replica, staging on production data), `?gundb_mode=write` only writes
  to it (a backup, an audit log); plain URLs sync both ways.
- **Offline-first.** Configured peers are re-dialled automatically. Writes
  made while offline are sent on reconnect, even after a restart, and
  subscriptions are restored.
- **Runs in the browser.** The package compiles to WebAssembly
  (`GOOS=js GOARCH=wasm`); `storage/idbstore` keeps a browser app's data in
  IndexedDB. See `examples/browser`.
- **Pluggable.** Storage is a two-method `Store` interface (in-memory by
  default). `storage/pebblestore` persists to disk on Pebble (an LSM tree)
  and keeps every accepted write, readable with `History` and `GetAt`.
  The transport is `transport.Conn`, with WebSocket built in
  (`transport/ws`).

## Command line

`cmd/gundb` is a relay server and a client for any GUN peer, Go or JS:

```sh
go install ella.to/gundb/cmd/gundb@latest

gundb server                                   # relay on http://localhost:8765/gun, in memory
gundb server --store pebble --data ./data --peers http://other:8765/gun
gundb server --max-nodes 1000 --max-bytes 10MB  # a quota for every user
gundb server --peers 'https://primary/gun?gundb_mode=read'  # a read replica; ?gundb_mode=write for a backup

gundb put users/1/name 'Ali'                   # write, and wait for the server
gundb get users/1                              # read, as JSON
gundb set todos '{"title":"buy milk"}'         # add to a list
gundb sub todos --map                          # print every change, Ctrl-C to stop
gundb put users/1/best --link users/2          # link to another node
gundb set users/1/friends --link users/2       # a list of links; unset removes one
gundb get users/1 --links                      # as stored: links unfollowed, souls shown
gundb get users -H "Authorization=Bearer 1234" # headers for your auth middleware

gundb user create -u ali -p secret123          # SEA user with its own key pair
gundb put '~/notes/1' 'secret' --enc -u ali -p secret123
gundb get '~/notes' --dec -u ali -p secret123
gundb put '~/shared/bob/wifi' 'hunter2' --enc --to bob -u ali -p secret123

gundb group init -u ali -p secret123           # encrypted group chat (a ratchet)
gundb group create team -u ali -p secret123
gundb group add team bob carol -u ali -p secret123
gundb group send team 'hi' -u ali -p secret123
gundb group sub ali/team -u bob -p …           # as bob
```

The [guide](GUIDE.md#14-the-gundb-command) walks through all of it, and
[SCENARIOS.md](SCENARIOS.md) has step-by-step scenarios to try it out.

`--addr` (or `GUNDB_ADDR`) picks the server, and `--timeout` bounds each
command (default 2s; 0 waits forever). Run `gundb <command> --help` for
the rest.

## Layout

| Path               | What                                                  |
| ------------------ | ----------------------------------------------------- |
| `.`                | the `gundb` package: `DB`, `Ref`, `Store`, protocol   |
| `transport/`       | the `Conn` / `Dialer` / `Listener` interfaces         |
| `transport/ws/`    | WebSocket adapter (default)                           |
| `sea/`             | SEA crypto: key pairs, sign, verify, encrypt, ECDH    |
| `sea/senderkey`    | group encryption with sender keys (a ratchet)         |
| `storage/memstore` | in-memory store that can list its souls               |
| `storage/pebblestore` | on-disk store with full history (Pebble)           |
| `storage/idbstore` | IndexedDB store for browsers (js/wasm only)           |
| `cmd/gundb`        | the `gundb` command: relay server and client          |
| `examples/`        | runnable programs used by the guide                   |
| `loadtest/`        | load generator: throughput, latency, cost per user    |
| `interop/`         | tests against the JavaScript GUN                      |

## Tests

```sh
go test -race ./...

# also run against real GUN.js (needs Node.js):
(cd interop/testdata && npm install) && go test ./interop

# and run the IndexedDB store's wasm tests in Node:
(cd storage/idbstore/testdata && npm install) && go test ./storage/idbstore
```

Load test a relay with simulated users (the relay runs in its own process;
pass `-store pebble` or `-store pebble-nosync` to test persistence):

```sh
go run ./loadtest -users 100,1000,10000 -duration 10s
```

## Not included (yet)

- **RAD storage.** There is no Go port of GUN's radix/radisk on-disk format.
  Persistence uses `storage/pebblestore` instead. Go peers still sync over
  the wire with JS relays that use radisk.
- **SEA certificates (`SEA.certify`).** You can't issue certificates, and
  signed fields that carry a certificate (`+`/`*`) fail verification. That
  means writes into another user's space are rejected.

## License

MIT
