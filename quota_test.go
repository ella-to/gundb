package gundb

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// pathOwner is the Owner of the quota tests: users/<name>/... is <name>'s.
func pathOwner(soul string) string {
	rest, ok := strings.CutPrefix(soul, "users/")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, "/")
	return name
}

// recount works out every owner's usage from scratch, the slow way.
func recount(t *testing.T, s SoulStore, owner func(string) string) map[string]Usage {
	t.Helper()
	ctx := context.Background()
	souls, err := s.ListSouls(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Usage{}
	for _, soul := range souls {
		o := owner(soul)
		if o == "" {
			continue
		}
		n, _ := s.Get(ctx, soul)
		u := out[o]
		live := false
		for k, v := range n.Fields {
			u.Bytes += fieldSize(k, v)
			live = live || isLive(v)
		}
		if live {
			u.Nodes++
		}
		if u != (Usage{}) {
			out[o] = u
		}
	}
	return out
}

// wholeNodeStore has only Get and Put: merges read and write whole nodes.
type wholeNodeStore struct{ s *memoryStore }

func (p wholeNodeStore) Get(ctx context.Context, soul string) (*Node, error) {
	return p.s.Get(ctx, soul)
}
func (p wholeNodeStore) Put(ctx context.Context, n *Node) error { return p.s.Put(ctx, n) }
func (p wholeNodeStore) ListSouls(ctx context.Context, prefix string) ([]string, error) {
	return p.s.ListSouls(ctx, prefix)
}

// fieldOnlyStore reads fields one by one but has no HasLive, so the quota
// falls back to Get.
type fieldOnlyStore struct{ s *memoryStore }

func (f fieldOnlyStore) Get(ctx context.Context, soul string) (*Node, error) {
	return f.s.Get(ctx, soul)
}
func (f fieldOnlyStore) Put(ctx context.Context, n *Node) error { return f.s.Put(ctx, n) }
func (f fieldOnlyStore) PutFields(ctx context.Context, soul string, n *Node) error {
	return f.s.PutFields(ctx, soul, n)
}
func (f fieldOnlyStore) GetFields(ctx context.Context, soul string, fields []string) (*Node, error) {
	return f.s.GetFields(ctx, soul, fields)
}
func (f fieldOnlyStore) ListSouls(ctx context.Context, prefix string) ([]string, error) {
	return f.s.ListSouls(ctx, prefix)
}

// TestQuotaCountsLikeARecount applies random writes (new fields,
// overwrites, deletes, links, several nodes at once) and checks after each
// one that the running totals match a recount of the store, for every kind
// of store the merge handles differently.
func TestQuotaCountsLikeARecount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func(*memoryStore) Store
	}{
		{"field reader with HasLive", func(m *memoryStore) Store { return m }},
		{"field reader without HasLive", func(m *memoryStore) Store { return fieldOnlyStore{m} }},
		{"whole nodes", func(m *memoryStore) Store { return wholeNodeStore{m} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := NewMemoryStore().(*memoryStore)
			db := New(Options{Store: tc.store(mem), Quota: &Quota{Owner: pathOwner}})
			defer db.Close()
			r := rand.New(rand.NewPCG(1, 2))
			state := float64(time.Now().UnixMilli()) - 1e7 // in the past: applied at once
			value := func() Value {
				switch r.IntN(8) {
				case 0, 1:
					return Null{} // deletes are common, to empty nodes
				case 6:
					return String(`{":":null,"~":"sig"}`) // a signed delete, as in a user's space
				case 7:
					return String(`{":":"hi","~":"sig"}`) // a signed value
				case 2:
					return String(strings.Repeat("x", r.IntN(40)))
				case 3:
					return Number(r.Float64())
				case 4:
					return Bool(r.IntN(2) == 0)
				}
				return Link{Soul: fmt.Sprintf("users/%c/n%d", 'a'+r.IntN(2), r.IntN(4))}
			}
			for i := range 3000 {
				g := graph{}
				for range 1 + r.IntN(3) {
					var soul string
					switch r.IntN(5) {
					case 0:
						soul = fmt.Sprintf("public/%d", r.IntN(3)) // nobody's
					default:
						soul = fmt.Sprintf("users/%c/n%d", 'a'+r.IntN(3), r.IntN(4))
					}
					n := g.node(soul)
					for range 1 + r.IntN(3) {
						state++
						n.Set(fmt.Sprintf("f%d", r.IntN(4)), value(), state)
					}
				}
				if _, err := db.apply(t.Context(), g, nil); err != nil {
					t.Fatal(err)
				}
				want := recount(t, mem, pathOwner)
				for _, owner := range []string{"a", "b", "c"} {
					if got := db.Usage(owner); got != want[owner] {
						t.Fatalf("after write %d: %s uses %+v, a recount says %+v", i, owner, got, want[owner])
					}
				}
				if len(db.Usages()) != len(want) {
					t.Fatalf("after write %d: usages %v, a recount says %v", i, db.Usages(), want)
				}
			}
		})
	}
}

func TestQuotaSizes(t *testing.T) {
	ctx := t.Context()
	db := New(Options{Quota: &Quota{Owner: pathOwner}})
	defer db.Close()
	note := db.Get("users").Get("ali").Get("notes").Get("1")
	if err := note.Put(ctx, map[string]any{"text": "hello", "n": 1, "ok": true}); err != nil {
		t.Fatal(err)
	}
	// text: 4+5, n: 1+8, ok: 2+1. The links are ali's too: notes in
	// users/ali, 5 + len("users/ali/notes"), and 1 in users/ali/notes,
	// 1 + len("users/ali/notes/1"). The link users.ali is in users,
	// which is nobody's.
	links := int64(5+15) + int64(1+17)
	want := Usage{Nodes: 3, Bytes: 9 + 9 + 3 + links}
	if got := db.Usage("ali"); got != want {
		t.Fatalf("usage %+v, want %+v", got, want)
	}
	note.Get("text").Put(ctx, "hi") // 3 bytes less
	want.Bytes -= 3
	if got := db.Usage("ali"); got != want {
		t.Fatalf("after overwrite: %+v, want %+v", got, want)
	}
	for _, k := range []string{"text", "n", "ok"} {
		note.Get(k).Put(ctx, nil)
	}
	want = Usage{Nodes: 2, Bytes: links}
	if got := db.Usage("ali"); got != want {
		t.Fatalf("after deleting every field: %+v, want %+v (the node is free)", got, want)
	}
	if got := db.Usage("nobody"); got != (Usage{}) {
		t.Fatalf("nobody's usage: %+v", got)
	}
}

// quotaRelay serves a relay whose users have plans: ali is on "free".
func quotaRelay(t *testing.T, free Limit) (*DB, string) {
	t.Helper()
	o := authRules()
	o.Wait = 300 * time.Millisecond
	o.Quota = &Quota{
		Owner: pathOwner,
		Limit: func(owner string) Limit {
			if owner == "ali" {
				return free
			}
			return Limit{} // everyone else: no limit
		},
	}
	relay := New(o)
	t.Cleanup(func() { relay.Close() })
	srv := httptest.NewServer(requireToken(relay))
	t.Cleanup(srv.Close)
	return relay, srv.URL + "/gun"
}

func TestQuotaRefusesWrites(t *testing.T) {
	ctx := t.Context()
	relay, url := quotaRelay(t, Limit{Nodes: 3})
	ali := client(t, relay, url, "tok-ali")
	notes := ali.Get("users").Get("ali").Get("notes")

	// users/ali, users/ali/notes and the first note: 3 nodes.
	if err := notes.Get("1").PutAck(ctx, map[string]string{"text": "one"}); err != nil {
		t.Fatalf("first note: %v", err)
	}
	if got := relay.Usage("ali").Nodes; got != 3 {
		t.Fatalf("ali has %d nodes, want 3", got)
	}
	before := relay.Usage("ali")
	err := notes.Get("2").PutAck(ctx, map[string]string{"text": "two"})
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("a 4th node: got %v, want ErrQuota", err)
	}
	if got := relay.Usage("ali"); got != before {
		t.Fatalf("a refused write changed the usage: %+v, was %+v", got, before)
	}
	if n, _ := relay.store.Get(ctx, "users/ali/notes/2"); n != nil {
		t.Fatalf("the relay stored a refused write: %+v", n)
	}
	// The link notes.2 came in the same write, so it was refused too.
	if n, _ := relay.store.Get(ctx, "users/ali/notes"); n.Fields["2"] != nil {
		t.Fatalf("half of a refused write was stored: %+v", n)
	}

	// Writing more into an existing node, and deleting, still work.
	if err := notes.Get("1").Get("text").PutAck(ctx, "one, edited"); err != nil {
		t.Fatalf("editing a note at the limit: %v", err)
	}
	if err := notes.Get("1").Get("text").PutAck(ctx, nil); err != nil {
		t.Fatalf("deleting at the limit: %v", err)
	}
	// That emptied note 1, which frees a node for note 2.
	if err := notes.Get("2").PutAck(ctx, map[string]string{"text": "two"}); err != nil {
		t.Fatalf("a new note after freeing one: %v", err)
	}

	// Bob has no limit; the relay's own writes are never refused.
	bob := client(t, relay, url, "tok-bob")
	for i := range 5 {
		if err := bob.Get("users").Get("bob").Get("notes").Get(fmt.Sprint(i)).PutAck(ctx, map[string]int{"n": i}); err != nil {
			t.Fatalf("bob: %v", err)
		}
	}
	if err := relay.Get("users").Get("ali").Get("notes").Get("3").PutAck(ctx, map[string]int{"n": 3}); err != nil && !errors.Is(err, ErrNoPeers) {
		t.Fatalf("the relay's own write: %v", err)
	}
	if got := relay.Usage("ali").Nodes; got != 4 {
		t.Fatalf("the relay's own write is still counted: ali has %d nodes, want 4", got)
	}
}

func TestQuotaRefusesBytes(t *testing.T) {
	ctx := t.Context()
	relay, url := quotaRelay(t, Limit{Bytes: 100})
	ali := client(t, relay, url, "tok-ali")
	bio := ali.Get("users").Get("ali").Get("bio")
	if err := bio.PutAck(ctx, strings.Repeat("a", 50)); err != nil {
		t.Fatal(err)
	}
	if err := bio.PutAck(ctx, strings.Repeat("a", 100)); !errors.Is(err, ErrQuota) {
		t.Fatalf("growing past 100 bytes: got %v, want ErrQuota", err)
	}
	if err := bio.PutAck(ctx, strings.Repeat("a", 10)); err != nil {
		t.Fatalf("shrinking: %v", err)
	}
}

// TestQuotaHoldsUnderConcurrency writes from many goroutines at once and
// checks the limit is never passed: the check and the count are one step.
func TestQuotaHoldsUnderConcurrency(t *testing.T) {
	ctx := t.Context()
	relay, url := quotaRelay(t, Limit{Nodes: 20})
	ali := client(t, relay, url, "tok-ali")
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := range 60 {
		wg.Go(func() {
			err := ali.Get("users/ali/n"+fmt.Sprint(i)).Get("v").PutAck(ctx, i)
			if err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			} else if !errors.Is(err, ErrQuota) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := relay.Usage("ali").Nodes; got != 20 || ok != 20 {
		t.Fatalf("%d writes accepted, ali has %d nodes; want 20 and 20", ok, got)
	}
	if want := recount(t, relay.store.(SoulStore), pathOwner)["ali"]; relay.Usage("ali") != want {
		t.Fatalf("usage %+v, a recount says %+v", relay.Usage("ali"), want)
	}
}

func TestQuotaLoadsStoredData(t *testing.T) {
	ctx := t.Context()
	store := NewMemoryStore()
	db := New(Options{Store: store})
	db.Get("users").Get("ali").Get("notes").Get("1").Put(ctx, map[string]string{"text": "one"})
	db.Get("users").Get("ali").Get("gone").Put(ctx, nil)
	db.Close()

	again := New(Options{Store: store, Quota: &Quota{Owner: pathOwner}})
	defer again.Close()
	want := recount(t, store.(SoulStore), pathOwner)["ali"]
	if got := again.Usage("ali"); got != want || got.Nodes != 3 {
		t.Fatalf("usage after a restart: %+v, want %+v with 3 nodes", got, want)
	}
}

func TestQuotaDefaultOwnerIsTheSEAUser(t *testing.T) {
	ctx := t.Context()
	db := New(Options{Quota: &Quota{}})
	defer db.Close()
	u, err := db.CreateUser(ctx, "ali", "secret123")
	if err != nil {
		t.Skipf("no SEA user: %v", err)
	}
	if err := u.Get("profile").Get("bio").PutAck(ctx, "hello"); err != nil && !errors.Is(err, ErrNoPeers) {
		t.Fatal(err)
	}
	got := db.Usage(u.Pub())
	if got.Nodes < 2 || got.Bytes == 0 {
		t.Fatalf("ali's usage: %+v, want her account and profile", got)
	}
	db.Get("public").Get("x").Put(ctx, 1)
	if len(db.Usages()) != 1 {
		t.Fatalf("public data is nobody's, got usages %v", db.Usages())
	}
}

// BenchmarkQuota compares writes with and without a quota: a field of a
// node with 10,000 fields, and Set into a growing list (a new field each
// time, so the quota looks whether the list was empty before).
func BenchmarkQuota(b *testing.B) {
	everything := &Quota{
		Owner: func(string) string { return "ali" },
		Limit: func(string) Limit { return Limit{Nodes: 1 << 40, Bytes: 1 << 40} },
	}
	for _, q := range []*Quota{nil, everything} {
		name := "quota=off"
		if q != nil {
			name = "quota=on"
		}
		b.Run("field/"+name, func(b *testing.B) {
			db := New(Options{Quota: q})
			defer db.Close()
			ctx := context.Background()
			fields := make(map[string]any, 10000)
			for i := range 10000 {
				fields["k"+fmt.Sprint(i)] = i
			}
			db.Get("list").Put(ctx, fields)
			ref := db.Get("list").Get("k0")
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				i++
				ref.Put(ctx, i)
			}
		})
		b.Run("set/"+name, func(b *testing.B) {
			db := New(Options{Quota: q})
			defer db.Close()
			ctx := context.Background()
			list := db.Get("list")
			b.ReportAllocs()
			for b.Loop() {
				list.Set(ctx, Todo{Title: "milk"})
			}
		})
	}
}
