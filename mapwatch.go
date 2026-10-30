package gundb

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
)

// mapWatch is the state of a Map().On subscription. It remembers which
// souls each item was read from, so a change recomputes only the items it
// touched: changing one item of a list of n costs the same for any n.
// It is used only by the subscription's own goroutine.
type mapWatch struct {
	path      []string
	list      string                     // soul of the node at path; "" if none
	pathSouls []string                   // souls read to resolve path, list included
	items     map[string]*mapItem        // by key
	bySoul    map[string]map[string]bool // soul -> keys of the items read from it
	refs      map[string]int             // soul -> how many of path and items read it
	touched   map[string]bool            // souls whose refs changed in this run
	last      map[string][]byte          // JSON last delivered per key ("null": deleted unseen)
}

type mapItem struct {
	val   Value    // the item's field in the list node
	souls []string // souls its value was read from
}

// mapChange is an item to deliver: its key and its value as JSON.
type mapChange struct {
	key  string
	data []byte
}

func newMapWatch(path []string) *mapWatch {
	return &mapWatch{
		path:    path,
		items:   map[string]*mapItem{},
		bySoul:  map[string]map[string]bool{},
		refs:    map[string]int{},
		touched: map[string]bool{},
		last:    map[string][]byte{},
	}
}

// update brings w up to date with what changed since the last run
// (rd.changed; nil means everything) and returns the items to deliver, in
// key order. It sets rd.delta to the souls to start or stop watching.
func (w *mapWatch) update(rd *reader) []mapChange {
	clear(w.touched)
	defer func() {
		rd.delta = make(map[string]bool, len(w.touched))
		for soul := range w.touched {
			rd.delta[soul] = w.refs[soul] > 0
		}
	}()
	changed := rd.changed
	full := changed == nil

	if full || anyIn(w.pathSouls, changed) {
		var v Value
		var err error
		souls := readSouls(rd, func() { v, err = rd.at(w.path) })
		l, isNode := v.(Link)
		if err != nil || !isNode {
			w.setPath(souls)
			w.dropItems()
			w.list = ""
			return nil
		}
		if !slices.Contains(souls, l.Soul) {
			souls = append(souls, l.Soul)
		}
		w.setPath(souls)
		if l.Soul != w.list { // a different node: start over
			w.dropItems()
			w.list, full = l.Soul, true
		}
	}
	if w.list == "" {
		return nil
	}

	dirty := map[string]bool{}
	if full || changed[w.list] {
		var n *Node
		var err error
		readSouls(rd, func() { n, err = rd.node(w.list) }) // watched as part of the path
		if err != nil || n == nil {
			return nil
		}
		for k, v := range n.Fields {
			it, ok := w.items[k]
			if !ok {
				it = &mapItem{}
				w.items[k] = it
			}
			if full || !ok || it.val != v {
				it.val, dirty[k] = v, true
			}
		}
		for k, it := range w.items {
			if _, ok := n.Fields[k]; !ok {
				w.setItem(k, it, nil)
				delete(w.items, k)
			}
		}
	}
	if !full {
		for soul := range changed {
			for k := range w.bySoul[soul] {
				dirty[k] = true
			}
		}
	}

	var out []mapChange
	for _, k := range slices.Sorted(maps.Keys(dirty)) {
		it := w.items[k]
		var t any
		var ok bool
		var err error
		w.setItem(k, it, readSouls(rd, func() { t, ok, err = rd.tree(it.val, map[string]bool{w.list: true}) }))
		if err != nil || !ok {
			continue // linked item not here yet; we are watching for it
		}
		prev, seen := w.last[k]
		if t == nil && !seen { // deleted before we ever saw it
			w.last[k] = []byte("null")
			continue
		}
		b, err := json.Marshal(t)
		if err != nil || bytes.Equal(b, prev) {
			continue
		}
		w.last[k] = b
		out = append(out, mapChange{k, b})
	}
	return out
}

// readSouls runs read with a fresh rd.seen and returns the souls it read.
func readSouls(rd *reader, read func()) []string {
	save := rd.seen
	rd.seen = map[string]bool{}
	read()
	souls := slices.Collect(maps.Keys(rd.seen))
	rd.seen = save
	return souls
}

func anyIn(souls []string, set map[string]bool) bool {
	for _, soul := range souls {
		if set[soul] {
			return true
		}
	}
	return false
}

func (w *mapWatch) ref(soul string, d int) {
	if w.refs[soul] += d; w.refs[soul] <= 0 {
		delete(w.refs, soul)
	}
	w.touched[soul] = true
}

func (w *mapWatch) setPath(souls []string) {
	for _, soul := range w.pathSouls {
		w.ref(soul, -1)
	}
	for _, soul := range souls {
		w.ref(soul, 1)
	}
	w.pathSouls = souls
}

// setItem records that item k was read from souls.
func (w *mapWatch) setItem(k string, it *mapItem, souls []string) {
	for _, soul := range it.souls {
		w.ref(soul, -1)
		if delete(w.bySoul[soul], k); len(w.bySoul[soul]) == 0 {
			delete(w.bySoul, soul)
		}
	}
	for _, soul := range souls {
		w.ref(soul, 1)
		if w.bySoul[soul] == nil {
			w.bySoul[soul] = map[string]bool{}
		}
		w.bySoul[soul][k] = true
	}
	it.souls = souls
}

func (w *mapWatch) dropItems() {
	for k, it := range w.items {
		w.setItem(k, it, nil)
	}
	clear(w.items)
}
