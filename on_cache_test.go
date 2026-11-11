package gundb

import (
	"testing"
	"time"
)

// recv returns the next value from ch, or fails after a while.
func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("no callback")
	}
	panic("unreachable")
}

// On re-renders only what changed; what it copies from earlier runs must
// stay current, and stay watched.
func TestOnKeepsUnchangedNodesCurrent(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	type kid struct {
		V   int  `json:"v"`
		Pet *kid `json:"pet,omitempty"`
	}
	doc := db.Get("doc")
	if err := doc.Put(ctx, map[string]kid{"a": {V: 1, Pet: &kid{V: 10}}, "b": {V: 2}}); err != nil {
		t.Fatal(err)
	}
	got := make(chan map[string]kid, 16)
	off := doc.On(func(m map[string]kid) { got <- m })
	defer off()
	recv(t, got)

	steps := []struct {
		ref  *Ref
		v    int
		want func(map[string]kid) bool
	}{
		{doc.Get("b").Get("v"), 20, func(m map[string]kid) bool { return m["b"].V == 20 && m["a"].Pet.V == 10 }},
		{doc.Get("a").Get("pet").Get("v"), 11, func(m map[string]kid) bool { return m["a"].Pet.V == 11 && m["b"].V == 20 }},
		{doc.Get("a").Get("pet").Get("v"), 12, func(m map[string]kid) bool { return m["a"].Pet.V == 12 }},
		{doc.Get("b").Get("v"), 21, func(m map[string]kid) bool { return m["b"].V == 21 && m["a"].Pet.V == 12 }},
	}
	for i, s := range steps {
		if err := s.ref.Put(ctx, s.v); err != nil {
			t.Fatal(err)
		}
		if m := recv(t, got); !s.want(m) {
			t.Fatalf("step %d: %+v", i, m)
		}
	}
}

// On through a cycle (alice.report.boss is alice) stays current: nodes
// whose JSON depends on the path are rendered again, not kept.
func TestOnThroughCycleWatchesKeptChildren(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	type person struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	alice, bob := db.Get("alice"), db.Get("bob")
	if err := alice.Put(ctx, map[string]any{"name": "Alice", "pet": map[string]any{"name": "Rex"}}); err != nil {
		t.Fatal(err)
	}
	if err := bob.Put(ctx, map[string]any{"name": "Bob", "boss": alice}); err != nil {
		t.Fatal(err)
	}
	if err := alice.Get("report").Put(ctx, bob); err != nil {
		t.Fatal(err)
	}
	// alice.report.boss is alice again.
	type withPet struct {
		Name string `json:"name"`
		Pet  person `json:"pet"`
	}
	got := make(chan withPet, 16)
	off := alice.Get("report").Get("boss").On(func(p withPet) { got <- p })
	defer off()
	recv(t, got)
	for _, name := range []string{"Max", "Bo", "Lu"} {
		if err := alice.Get("pet").Get("name").Put(ctx, name); err != nil {
			t.Fatal(err)
		}
		for p := recv(t, got); p.Pet.Name != name; p = recv(t, got) {
		}
		// An unrelated change to bob, on the path, keeps alice's JSON.
		if err := bob.Get("name").Put(ctx, "Bob "+name); err != nil {
			t.Fatal(err)
		}
	}
}
