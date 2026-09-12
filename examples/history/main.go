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
