package gundb

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"
)

// refTree is how reads built their result before treeJSON: a map tree,
// then json.Marshal. treeJSON must decode to the same value.
func refTree(r *reader, v Value, path map[string]bool) (any, bool, error) {
	if l, ok := v.(Link); ok {
		return refObject(r, l.Soul, path)
	}
	return refPlain(v), true, nil
}

func refObject(r *reader, soul string, path map[string]bool) (any, bool, error) {
	n, err := r.node(soul)
	if err != nil || n == nil {
		return nil, false, err
	}
	path[soul] = true
	defer delete(path, soul)
	out := map[string]any{}
	for k, v := range n.Fields {
		switch x := v.(type) {
		case Null:
		case Link:
			if path[x.Soul] || len(path) >= maxDepth {
				out[k] = refPlain(x)
				continue
			}
			child, ok, err := refObject(r, x.Soul, path)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				child = refPlain(x)
			}
			out[k] = child
		default:
			out[k] = refPlain(v)
		}
	}
	return out, true, nil
}

func refPlain(v Value) any {
	switch x := v.(type) {
	case Bool:
		return bool(x)
	case Number:
		return float64(x)
	case String:
		return string(x)
	case Link:
		return map[string]any{"#": x.Soul}
	}
	return nil
}

func TestTreeJSONMatchesReference(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	values := func(souls []string) Value {
		switch r.IntN(8) {
		case 0:
			return Null{}
		case 1:
			return Bool(r.IntN(2) == 0)
		case 2:
			return Number(r.NormFloat64() * 1e6)
		case 3:
			return Number(float64(r.Int64N(1 << 53)))
		case 4:
			return String([]string{"", "plain", `q"uote`, "<b>&amp;</b>", "tab\t\n", "é😀", "\x01"}[r.IntN(7)])
		default: // links, to existing souls, to missing ones, to themselves
			if r.IntN(5) == 0 {
				return Link{Soul: "missing"}
			}
			return Link{Soul: souls[r.IntN(len(souls))]}
		}
	}
	for round := range 300 {
		db := newDB(t)
		souls := make([]string, 1+r.IntN(6))
		for i := range souls {
			souls[i] = fmt.Sprint("n", i)
		}
		for _, soul := range souls {
			n := NewNode(soul)
			for f := range r.IntN(6) {
				n.Set(fmt.Sprint("f", f), values(souls), 1)
			}
			if err := db.store.Put(t.Context(), n); err != nil {
				t.Fatal(err)
			}
		}
		for _, start := range append([]Value{Link{Soul: "missing"}, Null{}, String("x")}, Link{Soul: souls[0]}) {
			rd := &reader{db: db, ctx: t.Context()}
			want, wok, werr := refTree(rd, start, map[string]bool{})
			b, ok, err := rd.treeJSON(nil, start, map[string]bool{})
			if ok != wok || (err == nil) != (werr == nil) {
				t.Fatalf("round %d from %v: ok %v/%v err %v/%v", round, start, ok, wok, err, werr)
			}
			if !ok {
				continue
			}
			wb, _ := json.Marshal(want)
			var got, wantAny any
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("round %d: %s: %v", round, b, err)
			}
			json.Unmarshal(wb, &wantAny)
			if !reflect.DeepEqual(got, wantAny) {
				t.Fatalf("round %d from %v:\n got %s\nwant %s", round, start, b, wb)
			}
		}
	}
}
