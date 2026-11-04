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

// at follows path from its root node through links and returns the value
// at its end. A root path yields Link{root}. nil means nothing is there.
func (r *reader) at(path []string) (Value, error) {
	var v Value = Link{Soul: path[0]}
	for _, key := range path[1:] {
		l, ok := v.(Link)
		if !ok {
			return nil, nil
		}
		n, err := r.node(l.Soul)
		if err != nil || n == nil {
			return nil, err
		}
		if v = n.Fields[key]; v == nil {
			return nil, nil
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
	n, err := r.node(soul)
	if err != nil || n == nil {
		return b, false, err
	}
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
		if l, ok := v.(Link); ok && !path[l.Soul] && len(path) < maxDepth {
			var found bool
			if b, found, err = r.objectJSON(b, l.Soul, path); err != nil {
				return nil, false, err
			}
			if found {
				continue
			}
			// The node isn't here (b is unchanged): write the link itself.
		}
		b = appendPlain(b, v)
	}
	return append(b, '}'), true, nil
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
		if n, _ := r.node(soul); n != nil {
			if l, ok := n.Fields[key].(Link); ok {
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
	var cur *Node
	loaded := false
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
		if child == "" && !loaded {
			var err error
			if cur, err = r.node(soul); err != nil {
				return err
			}
			loaded = true
		}
		if child == "" && cur != nil {
			if l, ok := cur.Fields[k].(Link); ok {
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
