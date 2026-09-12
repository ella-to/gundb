package pebblestore

import (
	"context"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"testing"

	"ella.to/gundb"
)

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sameNode(t *testing.T, got, want *gundb.Node) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
		return
	}
	if got == nil || got.Soul != want.Soul || !maps.Equal(got.Fields, want.Fields) || !maps.Equal(got.States, want.States) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestGetPut(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := open(t, dir)
	if n, err := s.Get(ctx, "nobody"); n != nil || err != nil {
		t.Fatalf("unknown soul: %+v, %v", n, err)
	}
	n := gundb.NewNode("a\x00b") // NUL in souls and fields must not confuse keys
	n.Set("name", gundb.String("Alice"), 1)
	n.Set("pet", gundb.Link{Soul: "pets/rex"}, 2)
	n.Set("age", gundb.Number(30.5), 3)
	n.Set("ok", gundb.Bool(true), 4)
	n.Set("no", gundb.Bool(false), 5)
	n.Set("gone", gundb.Null{}, 6)
	n.Set("x\x00y", gundb.String(""), -7)
	if err := s.Put(ctx, n); err != nil {
		t.Fatal(err)
	}
	other := gundb.NewNode("a") // a prefix of the first soul
	other.Set("k", gundb.String("v"), 1)
	s.Put(ctx, other)

	got, err := s.Get(ctx, "a\x00b")
	if err != nil {
		t.Fatal(err)
	}
	sameNode(t, got, n)
	got, _ = s.Get(ctx, "a")
	sameNode(t, got, other)

	s.Close()
	s = open(t, dir)
	got, _ = s.Get(ctx, "a\x00b")
	sameNode(t, got, n)
}

func TestHistoryAndGetAt(t *testing.T) {
	ctx := t.Context()
	s := open(t, t.TempDir())
	write := func(field string, v gundb.Value, state float64) {
		n := gundb.NewNode("doc")
		n.Set(field, v, state)
		if err := s.PutFields(ctx, "doc", n); err != nil {
			t.Fatal(err)
		}
	}
	write("title", gundb.String("draft"), 10)
	write("title", gundb.String("final"), 30)
	write("title", gundb.String("v2"), 20)
	write("title\x00", gundb.Number(1), 15) // sorts right after "title"
	write("body", gundb.String("hi"), 25)
	write("body", gundb.Null{}, 40) // deleted

	var states []float64
	var values []gundb.Value
	for v, err := range s.History(ctx, "doc", "title") {
		if err != nil {
			t.Fatal(err)
		}
		states = append(states, v.State)
		values = append(values, v.Value)
	}
	if !slices.Equal(states, []float64{30, 20, 10}) ||
		!slices.Equal(values, []gundb.Value{gundb.String("final"), gundb.String("v2"), gundb.String("draft")}) {
		t.Fatalf("History = %v %v", states, values)
	}

	at := func(t0 float64, kv ...any) *gundb.Node {
		if len(kv) == 0 {
			return nil
		}
		n := gundb.NewNode("doc")
		for i := 0; i < len(kv); i += 3 {
			n.Set(kv[i].(string), kv[i+1].(gundb.Value), float64(kv[i+2].(int)))
		}
		return n
	}
	for _, c := range []struct {
		t    float64
		want *gundb.Node
	}{
		{5, at(5)},
		{10, at(10, "title", gundb.String("draft"), 10)},
		{19, at(19, "title", gundb.String("draft"), 10, "title\x00", gundb.Number(1), 15)},
		{26, at(26, "title", gundb.String("v2"), 20, "title\x00", gundb.Number(1), 15, "body", gundb.String("hi"), 25)},
		{math.Inf(1), at(0, "title", gundb.String("final"), 30, "title\x00", gundb.Number(1), 15, "body", gundb.Null{}, 40)},
	} {
		got, err := s.GetAt(ctx, "doc", c.t)
		if err != nil {
			t.Fatal(err)
		}
		sameNode(t, got, c.want)
	}
}

func TestStateOrder(t *testing.T) {
	in := []float64{math.Inf(-1), -1e300, -2.5, -0.0, 0, 1e-300, 1, 1700000000000.5, math.Inf(1)}
	keys := make([]uint64, len(in))
	for i, f := range in {
		keys[i] = descending(f)
		if back := ascending(keys[i]); back != f {
			t.Errorf("ascending(descending(%v)) = %v", f, back)
		}
	}
	if !sort.SliceIsSorted(keys, func(i, j int) bool { return keys[i] > keys[j] }) {
		t.Fatalf("descending is not order-reversing: %x", keys)
	}
}

// Through the DB, history records every accepted write and nothing else.
func TestWithDB(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	db := gundb.New(gundb.Options{Store: s})
	defer db.Close()
	for _, name := range []string{"a", "b", "c"} {
		if err := db.Get("user").Get("name").Put(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := db.Get("user").Get("name").Once[string](ctx); err != nil || got != "c" {
		t.Fatalf("Once = %q, %v", got, err)
	}
	var names []gundb.Value
	for v, err := range s.History(ctx, "user", "name") {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, v.Value)
	}
	if !slices.Equal(names, []gundb.Value{gundb.String("c"), gundb.String("b"), gundb.String("a")}) {
		t.Fatalf("history = %v", names)
	}
}

func benchStore(b *testing.B, nodes int) *Store {
	s, err := Open(b.TempDir(), Options{NoSync: true})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	for i := range nodes {
		n := gundb.NewNode(fmt.Sprintf("users/user%06d", i))
		for f := range 5 {
			n.Set(fmt.Sprintf("field%d", f), gundb.String(fmt.Sprintf("value %d of user %d", f, i)), 1)
		}
		if err := s.Put(b.Context(), n); err != nil {
			b.Fatal(err)
		}
	}
	return s
}

// One field of a random node changes, as DB.apply writes it.
func BenchmarkPutFields(b *testing.B) {
	const nodes = 100000
	s := benchStore(b, nodes)
	r := rand.New(rand.NewPCG(1, 1))
	state := 2.0
	for b.Loop() {
		n := gundb.NewNode(fmt.Sprintf("users/user%06d", r.IntN(nodes)))
		n.Set("field0", gundb.String("changed"), state)
		if err := s.PutFields(b.Context(), n.Soul, n); err != nil {
			b.Fatal(err)
		}
		state++
	}
}

func BenchmarkGet(b *testing.B) {
	const nodes = 100000
	s := benchStore(b, nodes)
	r := rand.New(rand.NewPCG(2, 2))
	for b.Loop() {
		if _, err := s.Get(b.Context(), fmt.Sprintf("users/user%06d", r.IntN(nodes))); err != nil {
			b.Fatal(err)
		}
	}
}
