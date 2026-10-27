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
