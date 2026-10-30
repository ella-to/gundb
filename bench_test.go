package gundb

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"ella.to/gundb/sea"
)

// putFrame is a typical write from a JS peer: one node, a few fields.
const putFrame = `{"#":"J0XPPtCHI","put":{"users/alice":{"_":{"#":"users/alice",">":{"name":1700000000000,"age":1700000000000,"bio":1700000000000,"boss":1700000000000}},"name":"Alice","age":30,"bio":"Writes Go & JS <code> all day","boss":{"#":"users/bob"}}},"ok":{"@":9,"/":1}}`

func batchFrame(n int) []byte {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = strings.Replace(putFrame, "J0XPPtCHI", "J0XPPtC"+strconv.Itoa(i), 1)
	}
	return []byte("[" + strings.Join(parts, ",") + "]")
}

func BenchmarkReadFrame(b *testing.B) {
	for _, tc := range []struct {
		name  string
		frame []byte
	}{{"single", []byte(putFrame)}, {"batch10", batchFrame(10)}} {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.frame)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := readFrame(tc.frame); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkEncodePut(b *testing.B) {
	msgs, err := readFrame([]byte(putFrame))
	if err != nil {
		b.Fatal(err)
	}
	m := &msgs[0]
	b.ReportAllocs()
	for b.Loop() {
		if _, err := marshalWire(m); err != nil {
			b.Fatal(err)
		}
	}
}

// discardConn accepts and drops every frame.
type discardConn struct{ fakeConn }

func (discardConn) Send(context.Context, []byte) error { return nil }

// BenchmarkHandlePut is the relay's inbound write path: decode, merge, ack.
// Each iteration writes a newer state, so every put changes the store.
func BenchmarkHandlePut(b *testing.B) {
	db := New()
	defer db.Close()
	conn := &discardConn{*newFakeConn(false)}
	ctx, cancel := context.WithCancel(db.ctx)
	defer cancel()
	p := &peer{db: db, conn: conn, ctx: ctx, cancel: cancel, outWake: make(chan struct{}, 1), wants: map[string]bool{}}
	go p.writeLoop()
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		frame := strings.ReplaceAll(putFrame, "1700000000000", strconv.Itoa(1700000000000+i))
		frame = strings.Replace(frame, "J0XPPtCHI", strconv.Itoa(i), 1)
		msgs, err := readFrame([]byte(frame))
		if err != nil {
			b.Fatal(err)
		}
		db.handle(p, &msgs[0])
	}
}

func BenchmarkPutLocal(b *testing.B) {
	db := New()
	defer db.Close()
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if err := db.Get("users").Get("alice").Put(ctx, Member{Name: "Alice", Age: 30}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOnceLocal(b *testing.B) {
	db := New()
	defer db.Close()
	ctx := context.Background()
	if err := db.Get("users").Get("alice").Put(ctx, Member{Name: "Alice", Age: 30, Boss: &Member{Name: "Bob"}}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := db.Get("users").Get("alice").Once[Member](ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDupTrack(b *testing.B) {
	d := newDup(0)
	ids := make([]string, 4096)
	for i := range ids {
		ids[i] = randomID(9)
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		d.track(ids[i%len(ids)], nil)
		i++
	}
}

// BenchmarkPutFieldLargeNode writes one field of a node that already has
// n fields, which is what adding an item to a long list does.
func BenchmarkPutFieldLargeNode(b *testing.B) {
	for _, n := range []int{10, 1000, 10000} {
		b.Run("fields="+strconv.Itoa(n), func(b *testing.B) {
			db := New()
			defer db.Close()
			ctx := context.Background()
			fields := make(map[string]any, n)
			for i := range n {
				fields["k"+strconv.Itoa(i)] = i
			}
			if err := db.Get("list").Put(ctx, fields); err != nil {
				b.Fatal(err)
			}
			ref := db.Get("list").Get("k0")
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				i++
				if err := ref.Put(ctx, i); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkMapOnUpdate changes one item of a list of n items while
// Map().On watches the list, and waits for the callback that reports it.
func BenchmarkMapOnUpdate(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run("items="+strconv.Itoa(n), func(b *testing.B) {
			db := New()
			defer db.Close()
			ctx := context.Background()
			list := db.Get("list")
			items := make([]*Ref, n)
			for i := range n {
				var err error
				if items[i], err = list.Set(ctx, Todo{Title: "old " + strconv.Itoa(i)}); err != nil {
					b.Fatal(err)
				}
			}
			changed := make(chan string, 1)
			off := list.Map().On(func(_ string, t Todo) {
				if strings.HasPrefix(t.Title, "new ") {
					changed <- t.Title
				}
			})
			defer off()
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				i++
				title := "new " + strconv.Itoa(i)
				if err := items[i%n].Get("title").Put(ctx, title); err != nil {
					b.Fatal(err)
				}
				for got := <-changed; got != title; got = <-changed {
				}
			}
		})
	}
}

// BenchmarkOnPut changes one field of a node while On watches it, and
// waits for the callback.
func BenchmarkOnPut(b *testing.B) {
	db := New()
	defer db.Close()
	ctx := context.Background()
	alice := db.Get("alice")
	if err := alice.Put(ctx, Member{Name: "Alice", Boss: &Member{Name: "Bob"}}); err != nil {
		b.Fatal(err)
	}
	ages := make(chan int, 1)
	off := alice.On(func(m Member) { ages <- m.Age })
	defer off()
	<-ages
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		if err := alice.Get("age").Put(ctx, i); err != nil {
			b.Fatal(err)
		}
		for got := <-ages; got != i; got = <-ages {
		}
	}
}

// Profile10 is a struct with ten fields, for the SEA benchmarks.
type Profile10 struct {
	A, B, C, D, E, F, G, H, I, J string
}

func benchProfile() Profile10 {
	return Profile10{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet"}
}

func benchUser(b *testing.B, db *DB) *User {
	pair, err := sea.NewPair()
	if err != nil {
		b.Fatal(err)
	}
	return db.LoginPair(pair)
}

// BenchmarkUserPut is a signed write of ten fields into a user's space.
func BenchmarkUserPut(b *testing.B) {
	db := New()
	defer db.Close()
	ctx := context.Background()
	u := benchUser(b, db)
	b.ReportAllocs()
	for b.Loop() {
		if err := u.Get("profile").Put(ctx, benchProfile()); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkUserOnce reads ten signed fields back.
func BenchmarkUserOnce(b *testing.B) {
	db := New()
	defer db.Close()
	ctx := context.Background()
	u := benchUser(b, db)
	if err := u.Get("profile").Put(ctx, benchProfile()); err != nil {
		b.Fatal(err)
	}
	ref := db.User(u.Pub()).Get("profile")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ref.Once[Profile10](ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCheckSEA verifies a signed graph of ten fields, as a relay does
// for every signed put it receives.
func BenchmarkCheckSEA(b *testing.B) {
	db := New()
	defer db.Close()
	u := benchUser(b, db)
	t, err := toTree(benchProfile())
	if err != nil {
		b.Fatal(err)
	}
	g := graph{}
	rd := &reader{db: db, ctx: context.Background()}
	if err := rd.build(g, "~"+u.Pub()+"/profile", t.(map[string]any), db.state.Next()); err != nil {
		b.Fatal(err)
	}
	if _, err := u.sign(g); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := checkSEA(g, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGetFieldLargeNode is a relay answering a get for one field of a
// node with n fields.
func BenchmarkGetFieldLargeNode(b *testing.B) {
	for _, n := range []int{10, 10000} {
		b.Run("fields="+strconv.Itoa(n), func(b *testing.B) {
			db := New()
			defer db.Close()
			fields := make(map[string]any, n)
			for i := range n {
				fields["k"+strconv.Itoa(i)] = i
			}
			if err := db.Get("list").Put(context.Background(), fields); err != nil {
				b.Fatal(err)
			}
			conn := &discardConn{*newFakeConn(false)}
			ctx, cancel := context.WithCancel(db.ctx)
			defer cancel()
			p := &peer{db: db, conn: conn, ctx: ctx, cancel: cancel, outWake: make(chan struct{}, 1), wants: map[string]bool{}}
			go p.writeLoop()
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				i++
				msgs, err := readFrame([]byte(`{"#":"g` + strconv.Itoa(i) + `","get":{"#":"list",".":"k7"}}`))
				if err != nil {
					b.Fatal(err)
				}
				db.handle(p, &msgs[0])
			}
		})
	}
}

// BenchmarkFanout is a relay receiving a put for a node that 1,000 peers
// subscribe to, and queueing it for each of them. With "hide", CanRead
// hides one of the put's two nodes from every peer.
func BenchmarkFanout(b *testing.B) {
	for _, rules := range []string{"none", "allow", "hide"} {
		b.Run("canRead="+rules, func(b *testing.B) {
			var o Options
			switch rules {
			case "allow":
				o.CanRead = func(string, string) bool { return true }
			case "hide":
				o.CanRead = func(_, soul string) bool { return soul != "secret" }
			}
			db := New(o)
			defer db.Close()
			newPeer := func() *peer {
				ctx, cancel := context.WithCancel(db.ctx)
				b.Cleanup(cancel)
				p := &peer{db: db, conn: &discardConn{*newFakeConn(false)}, ctx: ctx, cancel: cancel, outWake: make(chan struct{}, 1), wants: map[string]bool{}}
				go p.writeLoop()
				return p
			}
			for range 1000 {
				newPeer().want("users/alice")
			}
			writer := newPeer()
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				i++
				state := strconv.Itoa(1700000000000 + i)
				frame := `{"#":"m` + strconv.Itoa(i) + `","put":{"users/alice":{"_":{"#":"users/alice",">":{"n":` + state + `}},"n":` + strconv.Itoa(i) +
					`},"secret":{"_":{"#":"secret",">":{"n":` + state + `}},"n":1}}}`
				msgs, err := readFrame([]byte(frame))
				if err != nil {
					b.Fatal(err)
				}
				db.handle(writer, &msgs[0])
			}
		})
	}
}
