package pebblestore

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"ella.to/gundb"
)

func TestHasLive(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	n := gundb.NewNode("n")
	n.Set("a", gundb.Null{}, 1)
	n.Set("b", gundb.String("x"), 1)
	n.Set("c", gundb.String(`{":":null,"~":"sig"}`), 1) // a signed delete
	if err := s.Put(ctx, n); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		soul string
		skip []string
		want bool
	}{
		{"n", nil, true},
		{"n", []string{"b"}, false},
		{"n", []string{"a", "c"}, true},
		{"missing", nil, false},
		{"n/child", nil, false}, // a soul that starts with n is another node
	} {
		got, err := s.HasLive(ctx, tc.soul, tc.skip)
		if err != nil || got != tc.want {
			t.Errorf("HasLive(%q, %v) = %v, %v; want %v", tc.soul, tc.skip, got, err, tc.want)
		}
	}
}

func owner(soul string) string {
	rest, ok := strings.CutPrefix(soul, "users/")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, "/")
	return name
}

// TestQuotaOnPebble writes random data through a DB with a quota, then
// reopens the store and checks the usage counted live, the usage rebuilt
// from disk, and a recount all agree.
func TestQuotaOnPebble(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	db := gundb.New(gundb.Options{Store: s, Quota: &gundb.Quota{Owner: owner}})
	r := rand.New(rand.NewPCG(3, 4))
	for range 500 {
		b := db.Batch() // several nodes in one write: the PutBatch path
		for range 1 + r.IntN(3) {
			ref := db.Get(fmt.Sprintf("users/%c/n%d", 'a'+r.IntN(3), r.IntN(5)))
			var v any = strings.Repeat("v", r.IntN(30))
			if r.IntN(3) == 0 {
				v = nil
			}
			if err := b.Put(ctx, ref.Get(fmt.Sprintf("f%d", r.IntN(3))), v); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	live := db.Usages()
	db.Close()
	s.Close()

	s = open(t, dir)
	again := gundb.New(gundb.Options{Store: s, Quota: &gundb.Quota{Owner: owner}})
	defer again.Close()
	want := map[string]gundb.Usage{}
	souls, _ := s.ListSouls(ctx, "")
	for _, soul := range souls {
		o := owner(soul)
		if o == "" {
			continue
		}
		n, _ := s.Get(ctx, soul)
		u, alive := want[o], false
		for k, v := range n.Fields {
			switch x := v.(type) {
			case gundb.String:
				u.Bytes += int64(len(k) + len(x))
				alive = true
			case gundb.Link:
				u.Bytes += int64(len(k) + len(x.Soul))
				alive = true
			}
		}
		if alive {
			u.Nodes++
		}
		if u != (gundb.Usage{}) {
			want[o] = u
		}
	}
	for _, o := range []string{"a", "b", "c"} {
		if live[o] != want[o] || again.Usage(o) != want[o] {
			t.Errorf("%s: counted %+v, reloaded %+v, recount %+v", o, live[o], again.Usage(o), want[o])
		}
	}
}
