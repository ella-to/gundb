// Package idbstore is a gundb.Store on IndexedDB, for Go compiled to
// WebAssembly and running in a browser (GOOS=js GOARCH=wasm). Data survives
// page reloads and browser restarts, so a web app keeps working offline and
// syncs what it wrote once a peer is reachable again.
//
//	store, err := idbstore.Open(ctx, "my-app")
//	db := gundb.New(gundb.Options{Store: store, Peers: peers})
//	defer store.Close()
//
// Each node is one record in the object store "nodes", keyed by its soul and
// holding the node's GUN wire JSON, so the data is readable in the browser's
// developer tools. Tabs of the same origin that open the same name share it.
//
// IndexedDB answers through browser callbacks, so the store blocks the
// calling goroutine until the browser replies. Never use a DB from inside a
// js.Func callback (a click handler, say) directly: start a goroutine, or
// the page deadlocks.
//
// The package only builds for js/wasm.
package idbstore
