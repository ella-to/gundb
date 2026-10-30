package gundb

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// mapLog records Map().On callbacks as "key=name" ("key=nil" if deleted).
type mapLog struct {
	mu  sync.Mutex
	got []string
}

func (l *mapLog) add(key string, m *Member) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if m == nil {
		l.got = append(l.got, key+"=nil")
		return
	}
	name := m.Name
	if m.Boss != nil {
		name += "/" + m.Boss.Name
	}
	l.got = append(l.got, key+"="+name)
}

// next waits until want arrives, and fails if any other callback came
// before it.
func (l *mapLog) next(t *testing.T, want string) {
	t.Helper()
	eventually(t, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.got) > 0
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.got[0] != want {
		t.Fatalf("got %v, want %s first", l.got, want)
	}
	l.got = l.got[1:]
}

// quiet fails if any callback arrives within a short while.
func (l *mapLog) quiet(t *testing.T) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.got) > 0 {
		t.Fatalf("unexpected callbacks %v", l.got)
	}
}

func TestMapOnRecomputesOnlyWhatChanged(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	team := db.Get("team")
	db.Get("bob").Put(ctx, Member{Name: "Bob"})
	db.Get("amy").Put(ctx, Member{Name: "Amy", Boss: &Member{Name: "Zed"}})
	db.Get("cat").Put(ctx, Member{Name: "Cat"})
	team.Get("b").Put(ctx, db.Get("bob"))
	team.Get("a").Put(ctx, db.Get("amy"))

	var l mapLog
	off := team.Map().On(l.add)
	l.next(t, "a=Amy/Zed")
	l.next(t, "b=Bob")
	l.quiet(t)

	// A change deep inside one item fires that item only.
	db.Get("amy").Get("boss").Get("name").Put(ctx, "Zoe")
	l.next(t, "a=Amy/Zoe")
	l.quiet(t)

	// Two items linking the same node both fire when it changes.
	team.Get("c").Put(ctx, db.Get("bob"))
	l.next(t, "c=Bob")
	db.Get("bob").Get("name").Put(ctx, "Rob")
	l.next(t, "b=Rob")
	l.next(t, "c=Rob")
	l.quiet(t)

	// Pointing an item at another node: the old one no longer matters.
	team.Get("c").Put(ctx, db.Get("cat"))
	l.next(t, "c=Cat")
	db.Get("bob").Get("name").Put(ctx, "Bobby")
	l.next(t, "b=Bobby")
	l.quiet(t)

	// An item whose node doesn't exist yet arrives when it does.
	team.Get("d").Put(ctx, db.Get("dan"))
	l.quiet(t)
	db.Get("dan").Put(ctx, Member{Name: "Dan"})
	l.next(t, "d=Dan")

	// Removing an item.
	team.Get("a").Put(ctx, nil)
	l.next(t, "a=nil")
	l.quiet(t)

	off() // the subscription's goroutine stops watching as it exits
	eventually(t, func() bool {
		db.watchMu.Lock()
		defer db.watchMu.Unlock()
		return len(db.watchers) == 0
	})
}

func TestMapOnFollowsTheParent(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	db.Get("old").Put(ctx, map[string]any{"x": map[string]any{"name": "Old X"}})
	db.Get("new").Put(ctx, map[string]any{"y": map[string]any{"name": "New Y"}})
	db.Get("app").Get("list").Put(ctx, db.Get("old"))

	var l mapLog
	off := db.Get("app").Get("list").Map().On(l.add)
	defer off()
	l.next(t, "x=Old X")

	// The path now leads to another node: its items are delivered, and the
	// old node's items no longer fire.
	db.Get("app").Get("list").Put(ctx, db.Get("new"))
	l.next(t, "y=New Y")
	db.Get("old").Get("x").Get("name").Put(ctx, "changed")
	l.quiet(t)
	db.Get("new").Get("y").Get("name").Put(ctx, "New Y2")
	l.next(t, "y=New Y2")
}

func TestMapOnManyItemsDeliversEachOnce(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	list := db.Get("list")
	var l mapLog
	off := list.Map().On(l.add)
	defer off()
	var want []string
	for i := range 50 {
		item, err := list.Set(ctx, Member{Name: fmt.Sprint(i)})
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, item.Key()+"="+fmt.Sprint(i))
	}
	eventually(t, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.got) >= len(want)
	})
	time.Sleep(50 * time.Millisecond) // and nothing more after that
	l.mu.Lock()
	defer l.mu.Unlock()
	got := slices.Clone(l.got)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("got %d callbacks %v,\nwant %d %v", len(got), got, len(want), want)
	}
}
