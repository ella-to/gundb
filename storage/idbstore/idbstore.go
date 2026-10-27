//go:build js && wasm

package idbstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall/js"

	"ella.to/gundb"
)

// objectStore is the IndexedDB object store that holds the nodes.
const objectStore = "nodes"

// Store is a gundb.SoulStore backed by one IndexedDB database. It is safe
// for concurrent use.
type Store struct {
	db    js.Value // IDBDatabase
	close func()
}

var _ gundb.SoulStore = (*Store)(nil)

// Open opens or creates the IndexedDB database name. It fails where
// IndexedDB is unavailable, e.g. in some private-browsing modes.
func Open(ctx context.Context, name string) (*Store, error) {
	idb := js.Global().Get("indexedDB")
	if !idb.Truthy() {
		return nil, errors.New("idbstore: indexedDB is not available")
	}
	var req js.Value
	if err := catch(func() { req = idb.Call("open", name, 1) }); err != nil {
		return nil, fmt.Errorf("idbstore: open %q: %w", name, err)
	}
	upgrade := js.FuncOf(func(js.Value, []js.Value) any {
		req.Get("result").Call("createObjectStore", objectStore)
		return nil
	})
	req.Set("onupgradeneeded", upgrade)
	db, err := await(ctx, req, "success", []string{"error", "blocked"}, upgrade)
	if err != nil {
		return nil, fmt.Errorf("idbstore: open %q: %w", name, err)
	}
	s := &Store{db: db}
	s.close = sync.OnceFunc(func() { db.Call("close") })
	// Another tab deleting or upgrading the database asks us to let go.
	onVersionChange := js.FuncOf(func(js.Value, []js.Value) any { s.close(); return nil })
	db.Set("onversionchange", onVersionChange)
	return s, nil
}

// Close closes the database. Calling it again does nothing.
func (s *Store) Close() error {
	s.close()
	return nil
}

// Get implements gundb.Store.
func (s *Store) Get(ctx context.Context, soul string) (*gundb.Node, error) {
	tx, err := s.tx("readonly")
	if err != nil {
		return nil, err
	}
	rec, err := await(ctx, tx.Call("objectStore", objectStore).Call("get", soul), "success", []string{"error"})
	if err != nil {
		return nil, fmt.Errorf("idbstore: get %q: %w", soul, err)
	}
	if rec.Type() != js.TypeString {
		return nil, nil
	}
	n := new(gundb.Node)
	if err := json.Unmarshal([]byte(rec.String()), n); err != nil {
		return nil, fmt.Errorf("idbstore: corrupt node %q: %w", soul, err)
	}
	return n, nil
}

// Put implements gundb.Store. It returns once the write is committed.
func (s *Store) Put(ctx context.Context, n *gundb.Node) error {
	b, err := json.Marshal(n)
	if err != nil {
		return fmt.Errorf("idbstore: put %q: %w", n.Soul, err)
	}
	tx, err := s.tx("readwrite")
	if err != nil {
		return err
	}
	if err := catch(func() { tx.Call("objectStore", objectStore).Call("put", string(b), n.Soul) }); err != nil {
		return fmt.Errorf("idbstore: put %q: %w", n.Soul, err)
	}
	if _, err := await(ctx, tx, "complete", []string{"error", "abort"}); err != nil {
		return fmt.Errorf("idbstore: put %q: %w", n.Soul, err)
	}
	return nil
}

// ListSouls implements gundb.SoulStore. Keys are sorted, so it walks a key
// cursor from prefix and stops at the first soul past it.
func (s *Store) ListSouls(ctx context.Context, prefix string) ([]string, error) {
	tx, err := s.tx("readonly")
	if err != nil {
		return nil, err
	}
	var req js.Value
	if err := catch(func() {
		from := js.Global().Get("IDBKeyRange").Call("lowerBound", prefix)
		req = tx.Call("objectStore", objectStore).Call("openKeyCursor", from)
	}); err != nil {
		return nil, fmt.Errorf("idbstore: list %q: %w", prefix, err)
	}
	var souls []string
	next := js.FuncOf(func(js.Value, []js.Value) any {
		cur := req.Get("result")
		if !cur.Truthy() {
			return nil
		}
		soul := cur.Get("key").String()
		if !strings.HasPrefix(soul, prefix) {
			return nil
		}
		souls = append(souls, soul)
		cur.Call("continue")
		return nil
	})
	req.Set("onsuccess", next)
	// The cursor's last success event comes before the transaction ends.
	if _, err := await(ctx, tx, "complete", []string{"error", "abort"}, next); err != nil {
		return nil, fmt.Errorf("idbstore: list %q: %w", prefix, err)
	}
	return souls, nil
}

// tx starts a transaction on the nodes object store.
func (s *Store) tx(mode string) (tx js.Value, err error) {
	if err := catch(func() { tx = s.db.Call("transaction", objectStore, mode) }); err != nil {
		return js.Value{}, fmt.Errorf("idbstore: %w", err)
	}
	return tx, nil
}

// await waits for target (a request or transaction) to fire its ok event
// and returns its result; any of the fail events is an error. keep holds
// other callbacks the operation uses; await releases them with its own once
// target has settled. If ctx ends first the operation still completes in
// the browser, but await returns ctx.Err() straight away.
func await(ctx context.Context, target js.Value, ok string, fail []string, keep ...js.Func) (js.Value, error) {
	type outcome struct {
		v   js.Value
		err error
	}
	done := make(chan outcome, 1)
	events := append([]string{ok}, fail...)
	for _, e := range events {
		h := js.FuncOf(func(js.Value, []js.Value) any {
			o := outcome{v: target.Get("result")}
			if e != ok {
				o = outcome{err: errorOf(target, e)}
			}
			select {
			case done <- o:
			default: // already settled, e.g. "abort" after "error"
			}
			return nil
		})
		keep = append(keep, h)
		target.Set("on"+e, h)
	}
	release := func() {
		// Detach first: a released js.Func panics if the browser calls it.
		for _, e := range events {
			target.Set("on"+e, js.Null())
		}
		for _, h := range keep {
			h.Release()
		}
	}
	select {
	case o := <-done:
		release()
		return o.v, o.err
	case <-ctx.Done():
		go func() {
			<-done
			release()
		}()
		return js.Value{}, ctx.Err()
	}
}

// errorOf describes why target failed.
func errorOf(target js.Value, event string) error {
	if e := target.Get("error"); e.Truthy() {
		return fmt.Errorf("%s: %s", e.Get("name").String(), e.Get("message").String())
	}
	return fmt.Errorf("%s", event)
}

// catch runs f and turns a JavaScript exception into an error.
func catch(f func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if jsErr, ok := r.(js.Error); ok {
				err = jsErr
				return
			}
			panic(r)
		}
	}()
	f()
	return nil
}
