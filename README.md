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
  LEX key queries. The [interop tests](interop/) run real GUN.js peers
  against Go peers in both directions.
- **Relays.** Any DB relays like a GUN.js server (AXE-style). Puts go to
  the peers that asked for those souls. Gets that miss locally are forwarded,
  and acks are routed back to whoever asked.
- **Permissions.** A relay can authenticate each connection and decide
  per user what it may read and write (`Authenticate`, `CanRead`,
  `CanWrite`), for Go and GUN.js clients alike.
- **Offline-first.** Configured peers are re-dialled automatically. Writes
  made while offline are sent on reconnect, and subscriptions are restored.
- **Pluggable.** Storage is a two-method `Store` interface (in-memory by
  default). `storage/pebblestore` persists to disk on Pebble (an LSM tree)
  and keeps every accepted write, readable with `History` and `GetAt`.
  The transport is `transport.Conn`, with WebSocket built in
  (`transport/ws`).

## Layout

| Path               | What                                                  |
| ------------------ | ----------------------------------------------------- |
| `.`                | the `gundb` package: `DB`, `Ref`, `Store`, protocol   |
| `transport/`       | the `Conn` / `Dialer` / `Listener` interfaces         |
| `transport/ws/`    | WebSocket adapter (default)                           |
| `storage/memstore` | in-memory store that can list its souls               |
| `storage/pebblestore` | on-disk store with full history (Pebble)           |
| `examples/`        | runnable programs used by the guide                   |
| `loadtest/`        | load generator: throughput, latency, cost per user    |
| `interop/`         | tests against the JavaScript GUN                      |

## Tests

```sh
go test -race ./...

# also run against real GUN.js (needs Node.js):
(cd interop/testdata && npm install) && go test ./interop
```

Load test a relay with simulated users (the relay runs in its own process;
pass `-store pebble` or `-store pebble-nosync` to test persistence):

```sh
go run ./loadtest -users 100,1000,10000 -duration 10s
```

## Not included (yet)

SEA (users and encryption), RAD storage, and LEX queries over souls.

## License

MIT
