package gundb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Ref points at a place in the graph, like a chain in GUN's JS API:
//
//	db.Get("users").Get("alice").Get("name")
//
// Creating a Ref does no work; methods read and write. A Ref is safe for
// concurrent use.
type Ref struct {
	db   *DB
	path []string
	user *User // signs writes into the user's space; nil for other refs
}

// Get returns a reference to the field key of this node.
func (r *Ref) Get(key string) *Ref {
	return &Ref{db: r.db, path: append(slices.Clip(r.path), key), user: r.user}
}

// Key returns the last key of the path (the soul, for a root ref).
func (r *Ref) Key() string { return r.path[len(r.path)-1] }

// String returns the path, e.g. "users.alice.name".
func (r *Ref) String() string { return strings.Join(r.path, ".") }

// Put writes v here and sends it to every peer. It does not wait for peers;
// use PutAck for that.
//
// v can be a struct, a map, a primitive (string, number, bool), nil (which
// deletes), or another *Ref (which stores a link to it). Structs and maps
// are merged field by field, never replaced, and nested structs and maps
// become their own linked nodes. A root ref (db.Get(key)) only takes
// structs and maps.
func (r *Ref) Put(ctx context.Context, v any) error { return r.put(ctx, v, false) }

// PutAck is Put that also waits until at least one peer acknowledges the
// write. It returns ErrNoPeers if there is nobody to ask; the write is
// still saved locally and sent when a peer connects.
func (r *Ref) PutAck(ctx context.Context, v any) error { return r.put(ctx, v, true) }

func (r *Ref) put(ctx context.Context, v any, ack bool) error {
	g := graph{}
	if err := r.addTo(ctx, g, v, r.db.state.Next()); err != nil {
		return err
	}
	return r.write(ctx, g, ack)
}

// addTo adds to g the nodes that putting v here writes.
func (r *Ref) addTo(ctx context.Context, g graph, v any, state float64) error {
	t, err := toTree(v)
	if err != nil {
		return err
	}
	for i := len(r.path) - 1; i > 0; i-- {
		t = map[string]any{r.path[i]: t}
	}
	obj, ok := t.(map[string]any)
	if _, isLink := linkOf(obj); !ok || isLink {
		return errRootPut(r.path[0])
	}
	rd := &reader{db: r.db, ctx: ctx, net: true}
	return rd.build(g, r.path[0], obj, state)
}

// write signs g if r belongs to a user, and writes it.
func (r *Ref) write(ctx context.Context, g graph, ack bool) error {
	var signed checked
	if r.user != nil {
		var err error
		if signed, err = r.user.sign(g); err != nil {
			return err
		}
	}
	return r.db.write(ctx, g, ack, signed)
}

// Set adds v to the unordered list stored at this ref, like gun.set in JS,
// and returns a ref to the new item. A struct or map becomes its own node,
// keyed by its soul; a *Ref is added as a link; anything else is stored
// under a random key. Read the list with Map().On or Once[map[string]T].
func (r *Ref) Set(ctx context.Context, v any) (*Ref, error) {
	t, err := toTree(v)
	if err != nil {
		return nil, err
	}
	m, isObj := t.(map[string]any)
	if !isObj {
		item := r.Get(uuid())
		return item, item.Put(ctx, t)
	}
	// The item and the link to it are one write.
	g := graph{}
	state := r.db.state.Next()
	soul, isLink := linkOf(m)
	if !isLink {
		if soul = metaSoul(m); soul == "" {
			soul = uuid()
			if r.user != nil { // like GUN, a user's items live in their space
				soul = "~" + r.user.Pub() + "/" + soul
			}
		}
		if err := r.root(soul).addTo(ctx, g, m, state); err != nil {
			return nil, err
		}
	}
	if err := r.Get(soul).addTo(ctx, g, Link{Soul: soul}, state); err != nil {
		return nil, err
	}
	return r.root(soul), r.write(ctx, g, false)
}

// Once reads the current value into T. Data not yet known locally is
// requested from peers first (waiting up to Options.Wait). Linked nodes are
// loaded too, so nested structs and maps come back filled in. It returns
// ErrNotFound if there is nothing here or the value was deleted.
//
//	user, err := db.Get("alice").Once[User](ctx)
//	name, err := db.Get("alice").Get("name").Once[string](ctx)
func (r *Ref) Once[T any](ctx context.Context) (T, error) {
	var zero T
	rd := &reader{db: r.db, ctx: ctx, net: true}
	v, err := rd.at(r.path)
	if err != nil {
		return zero, err
	}
	if _, deleted := v.(Null); v == nil || deleted {
		return zero, ErrNotFound
	}
	t, ok, err := rd.tree(v, map[string]bool{})
	if err != nil {
		return zero, err
	}
	if !ok {
		return zero, ErrNotFound
	}
	return decode[T](t)
}

// On calls fn with the value now and again every time it changes, whether
// the change is local or comes from a peer, until off is called or the DB
// is closed. Changes inside linked nodes count as changes too. A deleted
// value is delivered as T's zero value (nil for a pointer T).
//
// The first call carries the value as it was when On was called (On reads
// it before returning; it does not wait for peers). fn runs on a goroutine
// owned by this subscription, one call at a time. Changes that arrive while
// fn is busy may be merged: fn always gets the latest value, but not every
// intermediate one.
//
//	off := db.Get("alice").On(func(u User) { fmt.Println(u.Name) })
//	defer off()
func (r *Ref) On[T any](fn func(T)) (off func()) {
	var last []byte
	return r.db.watch(func(rd *reader) func() {
		v, err := rd.at(r.path)
		if err != nil || v == nil {
			return nil
		}
		t, ok, err := rd.tree(v, map[string]bool{})
		if err != nil || !ok || (t == nil && last == nil) {
			return nil
		}
		b, err := json.Marshal(t)
		if err != nil || bytes.Equal(b, last) {
			return nil
		}
		last = b
		var val T
		if err := json.Unmarshal(b, &val); err != nil {
			r.db.log.Warn("gundb: On: cannot decode", "ref", r.String(), "err", err)
			return nil
		}
		return func() { fn(val) }
	})
}

// root returns a ref to the node soul that signs like r does.
func (r *Ref) root(soul string) *Ref { return &Ref{db: r.db, path: []string{soul}, user: r.user} }

// Map returns a view over the items of the node at this ref (the entries of
// a Set, or the fields of any node), like gun.map() in JS.
func (r *Ref) Map() *MapRef { return &MapRef{ref: r} }

// MarshalJSON encodes the ref as a link, so a *Ref inside a struct you Put
// is stored as a reference to that node.
func (r *Ref) MarshalJSON() ([]byte, error) {
	rd := &reader{db: r.db, ctx: r.db.ctx}
	return Link{Soul: rd.soulOf(r.path)}.MarshalJSON()
}

// MapRef is the result of Ref.Map.
type MapRef struct{ ref *Ref }

// On calls fn(key, value) for every item now, and again for each item that
// is added, changed or deleted, until off is called. Like Ref.On, the first
// calls carry the items as they were when On was called. A deleted item is
// delivered as T's zero value (nil for a pointer T). For a Set, key is the
// item's soul: db.Get(key) refers to it.
//
//	off := db.Get("todos").Map().On(func(id string, t *Todo) { ... })
func (m *MapRef) On[T any](fn func(key string, v T)) (off func()) {
	r := m.ref
	w := newMapWatch(r.path)
	return r.db.watch(func(rd *reader) func() {
		type item struct {
			key string
			val T
		}
		var items []item
		for _, c := range w.update(rd) {
			var val T
			if err := json.Unmarshal(c.data, &val); err != nil {
				r.db.log.Warn("gundb: Map().On: cannot decode", "ref", r.String(), "key", c.key, "err", err)
				continue
			}
			items = append(items, item{c.key, val})
		}
		if len(items) == 0 {
			return nil
		}
		return func() {
			for _, it := range items {
				fn(it.key, it.val)
			}
		}
	})
}

func errRootPut(soul string) error {
	return fmt.Errorf("gundb: db.Get(%q) is a node: put a struct or map on it, or put a value on one of its fields with .Get(key)", soul)
}
