package gundb

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"ella.to/gundb/sea"
	"ella.to/gundb/sea/pathmap"
)

func newKey(t *testing.T) *pathmap.Key {
	t.Helper()
	k, err := pathmap.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// leaks fails if any soul or field db stores holds one of names.
func leaks(t *testing.T, db *DB, names ...string) {
	t.Helper()
	for n, err := range db.store.(ScanStore).Scan(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.Contains(n.Soul, name) {
				t.Fatalf("soul %q shows %q", n.Soul, name)
			}
			for k := range n.Fields {
				if strings.Contains(k, name) {
					t.Fatalf("%s.%s shows %q", n.Soul, k, name)
				}
			}
		}
	}
}

type Vault struct {
	Bank  string `json:"bank"`
	Notes *Note  `json:"notes,omitempty"`
}

type Note struct {
	Title string `json:"title"`
}

func TestSealedRoundTrip(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	key := newKey(t)
	vault := db.Get("scope").Sealed(key)
	if err := vault.Put(ctx, Vault{Bank: "IBAN", Notes: &Note{Title: "first"}}); err != nil {
		t.Fatal(err)
	}
	if err := vault.Get("notes").Get("title").Put(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	got, err := vault.Once[Vault](ctx)
	if err != nil || got.Bank != "IBAN" || got.Notes == nil || got.Notes.Title != "second" {
		t.Fatalf("read back %+v, %v", got, err)
	}
	if title, err := vault.Get("notes").Get("title").Once[string](ctx); err != nil || title != "second" {
		t.Fatalf("notes.title = %q, %v", title, err)
	}
	leaks(t, db, "bank", "notes", "title")

	// The same name gives another token in another node, and nothing reads
	// without the key.
	if _, err := db.Get("scope").Get("bank").Once[string](ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read without the key: %v", err)
	}
	if _, err := db.Get("scope").Sealed(newKey(t)).Get("bank").Once[string](ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read with another key: %v", err)
	}

	n, err := vault.Node(ctx)
	if err != nil || !slices.Equal(slices.Sorted(maps.Keys(n.Fields)), []string{"bank", "notes"}) {
		t.Fatalf("Node fields %v, %v", n, err)
	}
	links, err := vault.Links(ctx)
	if err != nil || len(links) != 1 || links["notes"] == "" || strings.Contains(links["notes"], "notes") {
		t.Fatalf("Links %v, %v", links, err)
	}
}

func TestSealedSubscriptions(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	key := newKey(t)
	list := db.Get("scope").Sealed(key).Get("todos")

	items := make(chan string, 16)
	off := list.Map().On(func(k string, todo Todo) { items <- k + "=" + todo.Title })
	defer off()
	whole := make(chan map[string]Todo, 16)
	off2 := db.Get("scope").Sealed(key).Get("todos").On(func(m map[string]Todo) { whole <- m })
	defer off2()

	if err := list.Get("milk").Put(ctx, Todo{Title: "buy milk"}); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, items); got != "milk=buy milk" {
		t.Fatalf("Map().On delivered %q", got)
	}
	for m := recv(t, whole); m["milk"].Title != "buy milk"; m = recv(t, whole) {
	}

	item, err := list.Set(ctx, Todo{Title: "call mum"})
	if err != nil {
		t.Fatal(err)
	}
	if got := recv(t, items); got != item.Key()+"=call mum" {
		t.Fatalf("Map().On delivered %q for a set item %s", got, item.Key())
	}
	if title, err := item.Get("title").Once[string](ctx); err != nil || title != "call mum" {
		t.Fatalf("set item title %q, %v", title, err)
	}
	leaks(t, db, "todos", "milk", "title")
}

// A ref to a sealed node, put as a value, links to the soul it has.
func TestSealedRefAsLink(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	key := newKey(t)
	scope := db.Get("scope").Sealed(key)
	if err := scope.Get("people").Get("bob").Put(ctx, Member{Name: "Bob"}); err != nil {
		t.Fatal(err)
	}
	if err := scope.Get("people").Get("alice").Put(ctx, map[string]any{"name": "Alice", "boss": scope.Get("people").Get("bob")}); err != nil {
		t.Fatal(err)
	}
	if boss, err := scope.Get("people").Get("alice").Get("boss").Get("name").Once[string](ctx); err != nil || boss != "Bob" {
		t.Fatalf("alice.boss.name = %q, %v", boss, err)
	}
	leaks(t, db, "people", "bob", "alice", "boss")
}

// A user's sealed scope goes through a relay, which checks every signature
// and stores only tokens; another device with the key reads it.
func TestSealedUserScopeThroughRelay(t *testing.T) {
	ctx := t.Context()
	relay := newDB(t)
	alice := newDB(t)
	link(t, relay, alice)
	pair, err := sea.NewPair()
	if err != nil {
		t.Fatal(err)
	}
	u := alice.LoginPair(pair)
	key := newKey(t)
	root, err := pathmap.NewRoot(u.Pub())
	if err != nil {
		t.Fatal(err)
	}
	secret, err := sea.Encrypt("IBAN DE89", key.ValueKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Soul(root).Sealed(key).Get("bank").PutAck(ctx, secret); err != nil {
		t.Fatal(err)
	}
	leaks(t, relay, "bank")

	other := newDB(t)
	link(t, relay, other)
	got, err := other.Get(root).Sealed(key).Get("bank").Once[string](ctx)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := sea.Decrypt(got, key.ValueKey())
	if err != nil || !bytes.Equal(plain, []byte(`"IBAN DE89"`)) {
		t.Fatalf("decrypted %s, %v", plain, err)
	}
	// The root is not linked from alice's node.
	if n, _ := relay.store.Get(context.Background(), "~"+u.Pub()); n != nil && len(n.Fields) > 0 {
		t.Fatalf("alice's node lists %v", slices.Collect(maps.Keys(n.Fields)))
	}
}

// BenchmarkSealed is the cost of sealing: the same three-key path written
// and read plain and through Ref.Sealed.
func BenchmarkSealed(b *testing.B) {
	key, err := pathmap.NewKey()
	if err != nil {
		b.Fatal(err)
	}
	for _, sealed := range []bool{false, true} {
		name := "plain"
		if sealed {
			name = "sealed"
		}
		db := New()
		defer db.Close()
		ctx := context.Background()
		ref := db.Get("scope")
		if sealed {
			ref = ref.Sealed(key)
		}
		ref = ref.Get("accounts").Get("main")
		b.Run("put/"+name, func(b *testing.B) {
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				i++
				if err := ref.Put(ctx, map[string]any{"bank": "IBAN", "n": i}); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("once/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := ref.Once[map[string]any](ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
