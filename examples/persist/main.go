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
