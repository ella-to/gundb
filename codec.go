package gundb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
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

// tree turns v into plain data ready for json.Marshal, loading linked nodes
// as nested objects. ok is false if v links to a node that doesn't exist.
func (r *reader) tree(v Value, path map[string]bool) (any, bool, error) {
	if l, ok := v.(Link); ok {
		return r.object(l.Soul, path)
	}
	return plain(v), true, nil
}

func (r *reader) object(soul string, path map[string]bool) (any, bool, error) {
	n, err := r.node(soul)
	if err != nil || n == nil {
		return nil, false, err
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
	out := make(map[string]any, len(n.Fields))
	for k, v := range n.Fields {
		switch x := v.(type) {
		case Null: // deleted
		case Link:
			if path[x.Soul] || len(path) >= maxDepth {
				out[k] = plain(x) // cycle: leave the link as {"#": soul}
				continue
			}
			child, ok, err := r.object(x.Soul, path)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				child = plain(x)
			}
			out[k] = child
		default:
			out[k] = plain(v)
		}
	}
	return out, true, nil
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

func decode[T any](v any) (T, error) {
	var t T
	b, err := json.Marshal(v)
	if err != nil {
		return t, fmt.Errorf("gundb: %w", err)
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return t, fmt.Errorf("gundb: decode into %T: %w", t, err)
	}
	return t, nil
}
