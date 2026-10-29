package memstore

import (
	"maps"
	"testing"

	"ella.to/gundb"
)

func TestFields(t *testing.T) {
	ctx := t.Context()
	s := New()
	if n, err := s.GetFields(ctx, "a", []string{"x"}); n != nil || err != nil {
		t.Fatalf("unknown soul: %+v, %v", n, err)
	}

	first := gundb.NewNode("a")
	first.Set("x", gundb.Number(1), 1)
	first.Set("y", gundb.Number(2), 1)
	if err := s.PutFields(ctx, "a", first); err != nil {
		t.Fatal(err)
	}
	first.Set("x", gundb.Number(99), 9) // the store must not keep the node it was given

	more := gundb.NewNode("a")
	more.Set("y", gundb.Number(3), 2)
	if err := s.PutFields(ctx, "a", more); err != nil {
		t.Fatal(err)
	}
	n, _ := s.Get(ctx, "a")
	if want := map[string]gundb.Value{"x": gundb.Number(1), "y": gundb.Number(3)}; !maps.Equal(n.Fields, want) || n.States["y"] != 2 {
		t.Fatalf("got %+v", n)
	}

	part, err := s.GetFields(ctx, "a", []string{"y", "missing"})
	if err != nil || len(part.Fields) != 1 || part.Fields["y"] != gundb.Number(3) || part.States["y"] != 2 {
		t.Fatalf("got %+v, %v", part, err)
	}
	part.Set("y", gundb.Number(0), 0) // nor share what it returns
	if n, _ := s.Get(ctx, "a"); n.Fields["y"] != gundb.Number(3) {
		t.Fatal("GetFields returned the stored node")
	}
	if n, err := s.GetFields(ctx, "a", []string{"missing"}); n != nil || err != nil {
		t.Fatalf("no stored field: %+v, %v", n, err)
	}
}
