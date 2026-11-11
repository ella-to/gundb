package gundb

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// offersTo returns the souls offered to a peer with pid by resubscribe.
func offersTo(t *testing.T, db *DB, pid string) map[string]bool {
	t.Helper()
	conn := newFakeConn(false)
	ctx, cancel := context.WithCancel(db.ctx)
	defer cancel()
	p := &peer{db: db, conn: conn, ctx: ctx, cancel: cancel, outWake: make(chan struct{}, 1), wants: map[string]bool{}}
	p.setPID(pid)
	db.resubscribe(p)
	go p.writeLoop()
	offered := map[string]bool{}
	for {
		select {
		case f := <-conn.frames:
			msgs, err := readFrame(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range msgs {
				for soul := range m.Put {
					offered[soul] = true
				}
			}
		case <-time.After(50 * time.Millisecond):
			return offered
		}
	}
}

// A peer that comes back is offered only what changed while it was away;
// a peer we do not know, everything.
func TestResubscribeOffersOnlyWhatChanged(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	for i := range 5 {
		soul := "n" + strconv.Itoa(i)
		if err := db.Get(soul).Put(ctx, map[string]any{"v": i}); err != nil {
			t.Fatal(err)
		}
		db.sync(ctx, soul) // read once: of interest
	}
	if got := offersTo(t, db, "stranger"); len(got) != 5 {
		t.Fatalf("a new peer was offered %v", got)
	}

	// relay left long ago; n3 changed since.
	db.interestMu.Lock()
	db.left["relay"] = time.Now().Add(-time.Hour)
	for _, in := range db.interest {
		in.changed = time.Now().Add(-2 * time.Hour)
	}
	db.interestMu.Unlock()
	if err := db.Get("n3").Get("v").Put(ctx, 33); err != nil {
		t.Fatal(err)
	}
	if got := offersTo(t, db, "relay"); len(got) != 1 || !got["n3"] {
		t.Fatalf("a peer back after an hour was offered %v, want n3", got)
	}

	// Writes made just before it left may not have reached it.
	db.interestMu.Lock()
	db.left["relay"] = time.Now()
	db.interestMu.Unlock()
	if got := offersTo(t, db, "relay"); !got["n3"] {
		t.Fatalf("a write made just before the peer left was not offered again: %v", got)
	}
}

// Interests nobody watches are dropped once idle; watched ones stay.
func TestIdleInterestIsDropped(t *testing.T) {
	oldIdle, oldSweep := interestIdle, sweepEvery
	interestIdle, sweepEvery = 20*time.Millisecond, 10*time.Millisecond
	defer func() { interestIdle, sweepEvery = oldIdle, oldSweep }()
	ctx := t.Context()
	db := newDB(t)
	if err := db.Get("watched").Put(ctx, map[string]any{"v": 1}); err != nil {
		t.Fatal(err)
	}
	off := db.Get("watched").On(func(map[string]any) {})
	defer off()
	if _, err := db.Get("read").Get("v").Once[int](ctx); err == nil {
		t.Fatal("read/v exists")
	}
	has := func(soul string) bool {
		db.interestMu.Lock()
		defer db.interestMu.Unlock()
		for k := range db.interest {
			if k.soul == soul {
				return true
			}
		}
		return false
	}
	eventually(t, func() bool { return has("watched") && !has("read") })
	db.interestMu.Lock()
	n := len(db.fieldInterest)
	db.interestMu.Unlock()
	if n != 0 {
		t.Fatalf("%d field interests counted after the sweep", n)
	}
}
