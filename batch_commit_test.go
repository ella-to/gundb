package gundb

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// All writes of a batch are saved in one PutBatch, and none before Commit.
func TestBatchCommitIsOneStoreBatch(t *testing.T) {
	ctx := t.Context()
	store := &batchStore{memoryStore: NewMemoryStore().(*memoryStore)}
	db := New(Options{Store: store})
	t.Cleanup(func() { db.Close() })

	b := db.Batch()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	check(b.Put(ctx, db.Get("a"), Profile{Name: "A"}))
	check(b.Put(ctx, db.Get("b"), Profile{Name: "B"}))
	_, err := b.Set(ctx, db.Get("a").Get("following"), db.Get("b"))
	check(err)
	_, err = b.Set(ctx, db.Get("b").Get("followers"), db.Get("a"))
	check(err)

	if _, err := db.Get("a").Once[Profile](ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("visible before Commit: %v", err)
	}
	if len(store.batches) != 0 {
		t.Fatalf("saved before Commit: %v", store.batches)
	}

	check(b.Commit(ctx))
	if want := [][]string{{"a", "a/following", "b", "b/followers"}}; !slices.EqualFunc(store.batches, want, slices.Equal) {
		t.Fatalf("batches %v, want %v", store.batches, want)
	}
	following, err := db.Get("a").Get("following").Links(ctx)
	if err != nil || following["b"] != "b" {
		t.Fatalf("a follows %v, %v", following, err)
	}
	followers, err := db.Get("b").Get("followers").Links(ctx)
	if err != nil || followers["a"] != "a" {
		t.Fatalf("b's followers %v, %v", followers, err)
	}
	if name, _ := db.Get("a").Get("following").Get("b").Get("name").Once[string](ctx); name != "B" {
		t.Fatalf("through the link: %q", name)
	}
	if b.Len() != 0 {
		t.Fatalf("batch not empty after Commit: %d", b.Len())
	}
}

// A failed Put or Set makes Commit write nothing, and the batch can be
// used again afterwards.
func TestBatchFailedWriteCommitsNothing(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	b := db.Batch()
	if err := b.Put(ctx, db.Get("a"), Profile{Name: "A"}); err != nil {
		t.Fatal(err)
	}
	bad := b.Put(ctx, db.Get("b"), 1) // a root takes only structs and maps
	if bad == nil {
		t.Fatal("putting a number at a root was accepted")
	}
	if err := b.Commit(ctx); err == nil || err.Error() != bad.Error() {
		t.Fatalf("Commit = %v, want %v", err, bad)
	}
	if _, err := db.Get("a").Once[Profile](ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a was written by a failed batch: %v", err)
	}

	if err := b.Put(ctx, db.Get("a"), Profile{Name: "A2"}); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(ctx); err != nil {
		t.Fatalf("reused batch: %v", err)
	}
	if p, _ := db.Get("a").Once[Profile](ctx); p.Name != "A2" {
		t.Fatalf("reused batch wrote %+v", p)
	}
}

func TestBatchLaterWriteWins(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	b := db.Batch()
	b.Put(ctx, db.Get("a").Get("name"), "first")
	b.Put(ctx, db.Get("a").Get("name"), "second")
	b.Put(ctx, db.Get("a").Get("city"), "Paris")
	if err := b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if p, _ := db.Get("a").Once[Profile](ctx); p != (Profile{Name: "second", City: "Paris"}) {
		t.Fatalf("got %+v", p)
	}
}

// Set the same ref twice in a batch, or in a batch and outside it: one entry.
func TestBatchSetSameRefOnce(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	following := db.Get("a").Get("following")
	if _, err := following.Set(ctx, db.Get("b")); err != nil {
		t.Fatal(err)
	}
	b := db.Batch()
	b.Set(ctx, following, db.Get("b"))
	b.Set(ctx, following, db.Get("b"))
	b.Set(ctx, following, db.Get("c"))
	if err := b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	links, _ := following.Links(ctx)
	if len(links) != 2 || links["b"] != "b" || links["c"] != "c" {
		t.Fatalf("links %v", links)
	}
}

func TestBatchRefFromOtherDB(t *testing.T) {
	ctx := t.Context()
	db, other := newDB(t), newDB(t)
	b := db.Batch()
	if err := b.Put(ctx, other.Get("a"), Profile{Name: "A"}); !errors.Is(err, ErrOtherDB) {
		t.Fatalf("Put = %v", err)
	}
	if _, err := b.Set(ctx, other.Get("list"), other.Get("a")); !errors.Is(err, ErrOtherDB) {
		t.Fatalf("Set = %v", err)
	}
	if err := b.Commit(ctx); !errors.Is(err, ErrOtherDB) {
		t.Fatalf("Commit = %v", err)
	}
}

func TestBatchEmptyCommit(t *testing.T) {
	db := newDB(t)
	if err := db.Batch().Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := db.Batch().CommitAck(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestBatchCommitAck(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	a, b := newDB(t), newDB(t)

	batch := a.Batch()
	batch.Put(ctx, a.Get("x"), map[string]any{"v": 1})
	if err := batch.CommitAck(ctx); !errors.Is(err, ErrNoPeers) {
		t.Fatalf("want ErrNoPeers, got %v", err)
	}

	link(t, a, b)
	batch.Put(ctx, a.Get("x"), map[string]any{"v": 2})
	batch.Put(ctx, a.Get("y"), map[string]any{"v": 3})
	if err := batch.CommitAck(ctx); err != nil {
		t.Fatal(err)
	}
	x, _ := b.Get("x").Get("v").Once[int](ctx)
	y, _ := b.Get("y").Get("v").Once[int](ctx)
	if x != 2 || y != 3 {
		t.Fatalf("b has x=%d y=%d", x, y)
	}
}

// A relay that refuses one write of a batch refuses all of it.
func TestBatchForbiddenWriteRejectsAll(t *testing.T) {
	ctx := t.Context()
	relay, url := authRelay(t)
	bob := client(t, relay, url, "tok-bob")

	b := bob.Batch()
	b.Put(ctx, bob.Get("users").Get("bob").Get("profile"), Profile{Name: "Bob"})
	b.Put(ctx, bob.Get("users").Get("ali").Get("profile"), Profile{Name: "hacked"})
	if err := b.CommitAck(ctx); !errors.Is(err, ErrForbidden) {
		t.Fatalf("CommitAck = %v, want ErrForbidden", err)
	}
	for _, soul := range []string{"users/bob/profile", "users/ali/profile", "users/bob", "users"} {
		if n, _ := relay.store.Get(ctx, soul); n != nil {
			t.Fatalf("relay saved %s from a refused batch: %+v", soul, n)
		}
	}

	// Without the forbidden write, the same batch goes through.
	b.Put(ctx, bob.Get("users").Get("bob").Get("profile"), Profile{Name: "Bob"})
	if err := b.CommitAck(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := relay.store.Get(ctx, "users/bob/profile"); n == nil || n.Fields["name"] != String("Bob") {
		t.Fatalf("relay holds %+v", n)
	}
}

// One batch can write into two users' spaces and a public node: each user
// signs their own nodes, and peers verify them.
func TestBatchMixesUsers(t *testing.T) {
	ctx := t.Context()
	relay, a, other := newDB(t), newDB(t), newDB(t)
	link(t, relay, a)
	link(t, relay, other)

	ali, err := a.CreateUser(ctx, "ali", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := a.CreateUser(ctx, "bob", "battery staple")
	if err != nil {
		t.Fatal(err)
	}
	b := a.Batch()
	b.Put(ctx, ali.Get("profile"), Profile{Name: "Ali"})
	b.Put(ctx, bob.Get("profile"), Profile{Name: "Bob"})
	b.Set(ctx, ali.Get("following"), bob.Root())
	b.Put(ctx, a.Get("public"), map[string]any{"note": "hello"})
	if err := b.CommitAck(ctx); err != nil {
		t.Fatal(err)
	}

	for pub, want := range map[string]string{ali.Pub(): "Ali", bob.Pub(): "Bob"} {
		p, err := other.User(pub).Get("profile").Once[Profile](ctx)
		if err != nil || p.Name != want {
			t.Fatalf("profile of %s = %+v, %v", want, p, err)
		}
		n, _ := relay.store.Get(ctx, "~"+pub+"/profile")
		if s, _ := n.Fields["name"].(String); !strings.HasPrefix(string(s), `{":":"`+want+`","~":"`) {
			t.Fatalf("relay stored %q unsigned", s)
		}
	}
	following, err := other.User(ali.Pub()).Get("following").Links(ctx)
	if err != nil || following["~"+bob.Pub()] != "~"+bob.Pub() {
		t.Fatalf("ali follows %v, %v", following, err)
	}
	if note, _ := other.Get("public").Get("note").Once[string](ctx); note != "hello" {
		t.Fatalf("public note %q", note)
	}
}

// A plain ref can't write into a user's space: the unsigned write is
// refused and nothing in the batch is applied.
func TestBatchUnsignedUserWriteRejectsAll(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	ali, err := db.CreateUser(ctx, "ali", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	b := db.Batch()
	b.Put(ctx, db.Get("public"), map[string]any{"note": "hello"})
	b.Put(ctx, db.User(ali.Pub()).Get("profile"), Profile{Name: "forged"})
	if err := b.Commit(ctx); err == nil {
		t.Fatal("unsigned write into a user's space was accepted")
	}
	if _, err := db.Get("public").Once[map[string]any](ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("public was written by a refused batch: %v", err)
	}
}
