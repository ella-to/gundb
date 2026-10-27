//go:build js && wasm

package idbstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"syscall/js"
	"testing"
	"time"

	"ella.to/gundb"
)

func open(t *testing.T, name string) *Store {
	t.Helper()
	if !js.Global().Get("indexedDB").Truthy() {
		t.Skip("no indexedDB; see run_test.go")
	}
	s, err := Open(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestGetPut(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.Name())
	if n, err := s.Get(ctx, "missing"); n != nil || err != nil {
		t.Fatalf("Get(missing) = %v, %v", n, err)
	}
	n := gundb.NewNode("users/ali")
	n.Set("name", gundb.String("Ali \"A\" 😀\n"), 1700000000000.5)
	n.Set("age", gundb.Number(30), 1)
	n.Set("admin", gundb.Bool(true), 2)
	n.Set("gone", gundb.Null{}, 3)
	n.Set("boss", gundb.Link{Soul: "users/bob"}, 4)
	if err := s.Put(ctx, n); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, n.Soul)
	if err != nil {
		t.Fatal(err)
	}
	if got.Soul != n.Soul || !maps.Equal(got.Fields, n.Fields) || !maps.Equal(got.States, n.States) {
		t.Fatalf("got %+v, want %+v", got, n)
	}
	n.Set("age", gundb.Number(31), 5)
	if err := s.Put(ctx, n); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, n.Soul); got.Fields["age"] != gundb.Number(31) {
		t.Fatalf("overwrite: age = %v", got.Fields["age"])
	}
}

func TestDataSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.Name())
	n := gundb.NewNode("kept")
	n.Set("x", gundb.Number(1), 1)
	if err := s.Put(ctx, n); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2 := open(t, t.Name())
	if got, err := s2.Get(ctx, "kept"); err != nil || got == nil || got.Fields["x"] != gundb.Number(1) {
		t.Fatalf("after reopen: %v, %v", got, err)
	}
}

func TestListSouls(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.Name())
	for _, soul := range []string{"chat/a", "chat/b", "chat", "chatter", "chat/é", "chat/😀", "other", "a"} {
		n := gundb.NewNode(soul)
		n.Set("x", gundb.Number(1), 1)
		if err := s.Put(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	for prefix, want := range map[string][]string{
		"chat/": {"chat/a", "chat/b", "chat/é", "chat/😀"},
		"chat":  {"chat", "chat/a", "chat/b", "chat/é", "chat/😀", "chatter"},
		"zzz":   nil,
		"":      {"a", "chat", "chat/a", "chat/b", "chat/é", "chat/😀", "chatter", "other"},
	} {
		got, err := s.ListSouls(ctx, prefix)
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("ListSouls(%q) = %q, want %q", prefix, got, want)
		}
	}
}

func TestConcurrentPuts(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.Name())
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			n := gundb.NewNode(fmt.Sprintf("n/%02d", i))
			n.Set("i", gundb.Number(i), 1)
			if err := s.Put(ctx, n); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	souls, err := s.ListSouls(ctx, "n/")
	if err != nil || len(souls) != 50 {
		t.Fatalf("ListSouls = %d souls, %v", len(souls), err)
	}
}

func TestCancelledContext(t *testing.T) {
	s := open(t, t.Name())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n := gundb.NewNode("late")
	n.Set("x", gundb.Number(1), 1)
	if err := s.Put(ctx, n); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put with cancelled ctx = %v", err)
	}
	// The write still lands, and its callbacks must not panic when it does.
	time.Sleep(50 * time.Millisecond)
	if got, _ := s.Get(context.Background(), "late"); got == nil {
		t.Fatal("cancelled write was lost")
	}
}

func TestWithDB(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.Name())
	db := gundb.New(gundb.Options{Store: s})
	type Member struct {
		Name string  `json:"name"`
		Boss *Member `json:"boss,omitempty"`
	}
	if err := db.Get("users").Get("ali").Put(ctx, Member{Name: "Ali", Boss: &Member{Name: "Bob"}}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// A new DB on the same database, like a page reload.
	db = gundb.New(gundb.Options{Store: open(t, t.Name())})
	defer db.Close()
	m, err := db.Get("users").Get("ali").Once[Member](ctx)
	if err != nil || m.Name != "Ali" || m.Boss == nil || m.Boss.Name != "Bob" {
		t.Fatalf("after reload: %+v, %v", m, err)
	}
	souls, err := db.Souls(ctx, gundb.Lex{Prefix: "users"})
	if err != nil || !slices.Equal(souls, []string{"users", "users/ali", "users/ali/boss"}) {
		t.Fatalf("Souls = %q, %v", souls, err)
	}
}
