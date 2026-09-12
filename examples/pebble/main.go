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
