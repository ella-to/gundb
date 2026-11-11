package gundb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"

	"ella.to/gundb/internal/js"
)

// maxDepth bounds how many links a read follows.
const maxDepth = 64

// maxFieldReads is how many fields of one node build reads one by one
// before it reads the whole node instead.
const maxFieldReads = 8

var errArray = errors.New("arrays are not supported by GUN; use Set to build a list")

// reader reads nodes for a Ref, either from the local store only (live
// subscriptions) or asking peers first (net: Once, Put).
type reader struct {
	db   *DB
	ctx  context.Context
	net  bool
	seen map[string]bool // souls read; set by watchers

	// For watchers: the souls changed since the last run (nil on the first
	// run: everything), and, for a compute that tracks its own souls, which
	// souls to start (true) or stop (false) watching.
	changed map[string]bool
	delta   map[string]bool

	// tree, if set, keeps the JSON of the nodes objectJSON writes, for the
	// next run of the same subscription (see treeCache). cuts counts the
	// links written as links because of the path they were reached by.
	tree *treeCache
	cuts int
}

func (r *reader) node(soul string) (*Node, error) {
	if r.seen != nil {
		r.seen[soul] = true
	}
	if r.net {
		r.db.sync(r.ctx, soul)
	}
	n, err := r.db.store.Get(r.ctx, soul)
	return unsign(n), err
}

// field reads one field of soul: from the store only that field if it can
// (FieldReader), and from peers only that field unless the whole node was
// asked for already. nil means it is not there.
func (r *reader) field(soul, key string) (Value, error) {
	if r.seen != nil {
		r.seen[soul] = true
	}
	if r.net {
		r.db.syncField(r.ctx, soul, key)
	}
	if ms, ok := r.db.store.(*memoryStore); ok { // the default: no copy at all
		return unsignValue(soul, ms.getField(soul, key)), nil
	}
	var n *Node
	var err error
	if fr, ok := r.db.store.(FieldReader); ok {
		n, err = fr.GetFields(r.ctx, soul, []string{key})
	} else {
		n, err = r.db.store.Get(r.ctx, soul)
	}
	if err != nil || n == nil {
		return nil, err
	}
	return unsignValue(soul, n.Fields[key]), nil
}

// at follows path from its root node through links and returns the value
// at its end. A root path yields Link{root}. nil means nothing is there.
// Each step reads one field, so a path through a large node costs the same
// as one through a small one.
func (r *reader) at(path []string) (Value, error) {
	var v Value = Link{Soul: path[0]}
	for _, key := range path[1:] {
		l, ok := v.(Link)
		if !ok {
			return nil, nil
		}
		var err error
		if v, err = r.field(l.Soul, key); err != nil || v == nil {
			return nil, err
		}
	}
	return v, nil
}

// treeJSON appends v as JSON, ready to decode into a user's type. Linked
// nodes are loaded and written as nested objects, fields sorted, deleted
// fields left out. A link that closes a cycle, or whose node isn't here,
// is written as {"#": soul}. ok is false if v itself links to a node that
// doesn't exist.
func (r *reader) treeJSON(b []byte, v Value, path map[string]bool) ([]byte, bool, error) {
	if l, ok := v.(Link); ok {
		return r.objectJSON(b, l.Soul, path)
	}
	return appendPlain(b, v), true, nil
}

func (r *reader) objectJSON(b []byte, soul string, path map[string]bool) ([]byte, bool, error) {
	cache := r.tree
	if r.net {
		cache = nil // a cache only knows what is stored here
	}
	if cache != nil {
		if f := cache.frags[soul]; f != nil {
			cache.seen(r, soul)
			return append(b, f.json...), true, nil
		}
	}
	n, err := r.node(soul)
	if err != nil || n == nil {
		return b, false, err
	}
	start, cuts := len(b), r.cuts
	var children []string
	if r.net { // fetch all children in parallel before walking them
		var wg sync.WaitGroup
		for _, v := range n.Fields {
			if l, ok := v.(Link); ok && !path[l.Soul] {
				wg.Go(func() { r.db.sync(r.ctx, l.Soul) })
			}
		}
		wg.Wait()
	}
	path[soul] = true
	defer delete(path, soul)
	b = append(b, '{')
	first := true
	for _, k := range slices.Sorted(maps.Keys(n.Fields)) {
		v := n.Fields[k]
		if _, deleted := v.(Null); deleted {
			continue
		}
		if !first {
			b = append(b, ',')
		}
		first = false
		b = append(js.AppendQuote(b, k), ':')
		if l, ok := v.(Link); ok {
			if cache != nil {
				children = append(children, l.Soul)
			}
			if !path[l.Soul] && len(path) < maxDepth {
				var found bool
				if b, found, err = r.objectJSON(b, l.Soul, path); err != nil {
					return nil, false, err
				}
				if found {
					continue
				}
				// The node isn't here (b is unchanged): write the link itself.
			} else {
				r.cuts++ // a cycle or too deep: depends on the path
			}
		}
		b = appendPlain(b, v)
	}
	b = append(b, '}')
	if cache != nil && r.cuts == cuts {
		cache.put(soul, b[start:], children)
	}
	return b, true, nil
}

// treeCache keeps, for one Ref.On subscription, the JSON objectJSON wrote
// for each node, so that a change re-renders only the nodes it touched and
// the nodes that embed them; the rest is copied. A node whose JSON depends
// on the path it was reached by (a link cut because of a cycle or the
// depth limit) is not kept. It is used only by the subscription's own
// goroutine.
type treeCache struct {
	frags   map[string]*frag
	parents map[string]map[string]bool // soul -> kept nodes that link to it
	visited map[string]bool            // souls seen has marked in this run
}

type frag struct {
	json     []byte
	children []string // the souls the node links to
}

func newTreeCache() *treeCache {
	return &treeCache{frags: map[string]*frag{}, parents: map[string]map[string]bool{}, visited: map[string]bool{}}
}

// update starts a run: it drops what changed souls invalidate. nil means
// everything.
func (c *treeCache) update(changed map[string]bool) {
	clear(c.visited)
	if changed == nil {
		clear(c.frags)
		clear(c.parents)
		return
	}
	for soul := range changed {
		c.drop(soul)
	}
}

// drop forgets soul and every kept node that embeds it.
func (c *treeCache) drop(soul string) {
	if f, ok := c.frags[soul]; ok {
		delete(c.frags, soul)
		for _, child := range f.children {
			if ps := c.parents[child]; ps != nil {
				if delete(ps, soul); len(ps) == 0 {
					delete(c.parents, child)
				}
			}
		}
	}
	for p := range c.parents[soul] {
		c.drop(p) // also removes p from c.parents[soul]
	}
}

func (c *treeCache) put(soul string, json []byte, children []string) {
	c.frags[soul] = &frag{json: bytes.Clone(json), children: children}
	for _, child := range children {
		if c.parents[child] == nil {
			c.parents[child] = map[string]bool{}
		}
		c.parents[child][soul] = true
	}
}

// seen marks soul and everything its kept JSON was read from as read by r,
// so the watcher keeps watching them.
func (c *treeCache) seen(r *reader, soul string) {
	if r.seen == nil || c.visited[soul] {
		return
	}
	c.visited[soul] = true
	r.seen[soul] = true
	if f := c.frags[soul]; f != nil {
		for _, child := range f.children {
			c.seen(r, child)
		}
	}
}

// appendPlain appends the JSON of one value; a link as {"#": soul}.
func appendPlain(b []byte, v Value) []byte {
	switch x := v.(type) {
	case Bool:
		if x {
			return append(b, "true"...)
		}
		return append(b, "false"...)
	case Number:
		return js.AppendNumber(b, float64(x))
	case String:
		return js.AppendQuote(b, string(x))
	case Link:
		return append(js.AppendQuote(append(b, `{"#":`...), x.Soul), '}')
	}
	return append(b, "null"...)
}

// soulOf returns the soul a node ref resolves to locally, or the soul a Put
// would create for it: the last linked soul on the path plus the remaining
// keys joined by "/" (how the reference names nested nodes).
func (r *reader) soulOf(path []string) string {
	soul := path[0]
	for i, key := range path[1:] {
		if v, _ := r.field(soul, key); v != nil {
			if l, ok := v.(Link); ok {
				soul = l.Soul
				continue
			}
		}
		return soul + "/" + strings.Join(path[i+1:], "/")
	}
	return soul
}

// fetchRefs reads from peers, under ctx, the nodes on the path of every
// *Ref in v, all at once. Encoding v then links each ref to the soul its
// node really has: Ref.MarshalJSON cannot wait for the network, so on its
// own it only sees what this DB has already read, and a ref to a node it
// has not (a Set item or a GUN.js node, with a random soul) would link to
// the soul its path would get if it were new.
func fetchRefs(ctx context.Context, v any) {
	switch v.(type) {
	case nil, string, bool, float64, int, int64, json.Number:
		return // the common values, without asking reflect
	}
	if t := reflect.TypeOf(v); t != refType && !mayHoldRef(t) {
		return
	}
	var wg sync.WaitGroup
	refsIn(reflect.ValueOf(v), 0, func(r *Ref) {
		if len(r.path) > 1 { // a root ref's soul is its key
			wg.Go(func() { (&reader{db: r.db, ctx: ctx, net: true}).soulOf(r.path) })
		}
	})
	wg.Wait()
}

var (
	refType       = reflect.TypeFor[*Ref]()
	marshalerType = reflect.TypeFor[json.Marshaler]()
	holdsRef      sync.Map // reflect.Type -> bool, see mayHoldRef
)

// refsIn calls found for every *Ref that encoding v as JSON would encode:
// in pointers, interfaces, maps and the fields encoding/json writes.
func refsIn(v reflect.Value, depth int, found func(*Ref)) {
	if !v.IsValid() || depth > 1000 { // deeper, encoding/json reports a cycle
		return
	}
	if v.Type() == refType {
		if !v.IsNil() && v.CanInterface() {
			found(v.Interface().(*Ref))
		}
		return
	}
	if !mayHoldRef(v.Type()) {
		return
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if !v.IsNil() {
			refsIn(v.Elem(), depth+1, found)
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			refsIn(it.Value(), depth+1, found)
		}
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			if encoded(t.Field(i)) {
				refsIn(v.Field(i), depth+1, found)
			}
		}
	}
}

// mayHoldRef reports whether a value of type t can contain a *Ref, so
// values that cannot (most structs) are not walked.
func mayHoldRef(t reflect.Type) bool {
	if b, ok := holdsRef.Load(t); ok {
		return b.(bool)
	}
	b := reachesRef(t, map[reflect.Type]bool{})
	holdsRef.Store(t, b) // only t's answer: those of types seen on the way can be cut short by seen
	return b
}

func reachesRef(t reflect.Type, seen map[reflect.Type]bool) bool {
	if t == refType {
		return true
	}
	if seen[t] {
		return false
	}
	seen[t] = true
	if t.Kind() != reflect.Interface && t.Implements(marshalerType) {
		return false // it encodes itself
	}
	switch t.Kind() {
	case reflect.Interface:
		return true
	case reflect.Pointer, reflect.Map:
		return reachesRef(t.Elem(), seen)
	case reflect.Struct:
		for i := range t.NumField() {
			if f := t.Field(i); encoded(f) && reachesRef(f.Type, seen) {
				return true
			}
		}
	}
	return false // slices and arrays are refused by GUN anyway
}

// encoded reports whether encoding/json writes the struct field f.
func encoded(f reflect.StructField) bool {
	return (f.IsExported() || f.Anonymous) && f.Tag.Get("json") != "-"
}

// build adds obj to g as node soul. Nested objects become their own nodes,
// linked from the parent: an existing link is reused, otherwise the child
// gets the soul "parent/key", exactly like gun.put does.
func (r *reader) build(g graph, soul string, obj map[string]any, state float64) error {
	n := g.node(soul)
	// The links of soul's nested objects: read one field per object, and
	// the whole node once when there are many (one get instead of many).
	var cur *Node
	reads := 0
	for k, v := range obj {
		if k == "_" { // metadata of a node read back from GUN
			continue
		}
		m, isObj := v.(map[string]any)
		if !isObj {
			val, err := leaf(v)
			if err != nil {
				return fmt.Errorf("gundb: %s.%s: %w", soul, k, err)
			}
			n.Set(k, val, state)
			continue
		}
		if s, ok := linkOf(m); ok {
			n.Set(k, Link{Soul: s}, state)
			continue
		}
		child := metaSoul(m)
		if child == "" {
			var old Value
			var err error
			if reads++; reads <= maxFieldReads {
				old, err = r.field(soul, k)
			} else {
				if reads == maxFieldReads+1 {
					cur, err = r.node(soul)
				}
				if cur != nil {
					old = cur.Fields[k]
				}
			}
			if err != nil {
				return err
			}
			if l, ok := old.(Link); ok {
				child = l.Soul
			}
		}
		if child == "" {
			child = soul + "/" + k
		}
		n.Set(k, Link{Soul: child}, state)
		if err := r.build(g, child, m, state); err != nil {
			return err
		}
	}
	return nil
}

// toTree turns any Go value into plain JSON data via encoding/json, so json
// tags, omitempty and custom marshalers all apply. Numbers stay json.Number.
func toTree(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("gundb: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("gundb: %w", err)
	}
	return out, nil
}

func leaf(v any) (Value, error) {
	switch x := v.(type) {
	case nil:
		return Null{}, nil
	case bool:
		return Bool(x), nil
	case string:
		return String(x), nil
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return nil, err
		}
		return Number(f), nil
	case []any:
		return nil, errArray
	}
	return nil, fmt.Errorf("unsupported value %T", v)
}

// linkOf reports whether m is a link: exactly {"#": "soul"}.
func linkOf(m map[string]any) (string, bool) {
	s, ok := m["#"].(string)
	return s, ok && s != "" && len(m) == 1
}

// metaSoul returns the soul in a node's "_" metadata, if any.
func metaSoul(m map[string]any) string {
	meta, _ := m["_"].(map[string]any)
	s, _ := meta["#"].(string)
	return s
}
