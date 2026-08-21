// Package gundb is GUN (https://github.com/amark/gun) for Go: a realtime,
// offline-first graph database that syncs with other peers, Go or
// JavaScript, over the GUN wire protocol.
//
// The API mirrors GUN's JS chain, typed with generic methods:
//
//	db := gundb.New(gundb.Options{Peers: []string{"ws://localhost:8765/gun"}})
//	defer db.Close()
//
//	alice := db.Get("users").Get("alice")
//	err := alice.Put(ctx, User{Name: "Alice"})          // write (merges fields)
//	user, err := alice.Once[User](ctx)                  // read once
//	off := alice.On(func(u User) { fmt.Println(u) })    // live updates
//	item, err := db.Get("todos").Set(ctx, Todo{...})    // add to a list
//	db.Get("todos").Map().On(func(id string, t Todo) {})
//
// A DB is also an http.Handler that accepts WebSocket peers, so serving
// browsers running GUN.js takes one line:
//
//	http.Handle("/gun", db)
//
// See GUIDE.md and the examples directory for complete programs.
package gundb
