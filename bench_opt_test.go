package gundb

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Benchmarks for the optimizations measured in report.md. Each names the
// optimization it measures.

// benchPeer returns a peer whose frames are dropped. With register it is
// also one of db's connected peers, as an inbound connection.
func benchPeer(b *testing.B, db *DB, register bool) *peer {
	b.Helper()
	ctx, cancel := context.WithCancel(db.ctx)
	b.Cleanup(cancel)
	p := &peer{db: db, conn: &discardConn{*newFakeConn(false)}, ctx: ctx, cancel: cancel, id: randomID(16),
		outWake: make(chan struct{}, 1), wants: map[string]bool{}}
	go p.writeLoop()
	if register {
		db.peersMu.Lock()
		db.peers[p] = struct{}{}
		db.peersMu.Unlock()
	}
	return p
}

// signedFrame returns a put of ten fields signed by u at state, as JSON
// with "ID" in place of the message ID.
func signedFrame(b *testing.B, db *DB, u *User, state float64) string {
	b.Helper()
	t, err := toTree(benchProfile())
	if err != nil {
		b.Fatal(err)
	}
	g := graph{}
	rd := &reader{db: db, ctx: context.Background()}
	if err := rd.build(g, "~"+u.Pub()+"/profile", t.(map[string]any), state); err != nil {
		b.Fatal(err)
	}
	if _, err := u.sign(g); err != nil {
		b.Fatal(err)
	}
	raw, err := (&message{ID: "ID", Put: g}).appendWire(nil)
	if err != nil {
		b.Fatal(err)
	}
	return string(raw)
}

// BenchmarkHandlePutSigned is a relay receiving a signed put of ten fields
// (optimization 1). "new" writes newer values, which must be verified;
// "known" sends what the relay already has, as offer and resubscribe do on
// every reconnect.
func BenchmarkHandlePutSigned(b *testing.B) {
	b.Run("new", func(b *testing.B) {
		db := New()
		defer db.Close()
		signer := New()
		defer signer.Close()
		u := benchUser(b, signer)
		p := benchPeer(b, db, false)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			b.StopTimer()
			i++
			frame := strings.Replace(signedFrame(b, signer, u, signer.state.Next()), `"ID"`, `"n`+strconv.Itoa(i)+`"`, 1)
			b.StartTimer()
			msgs, err := readFrame([]byte(frame))
			if err != nil {
				b.Fatal(err)
			}
			db.handle(p, &msgs[0])
		}
	})
	b.Run("known", func(b *testing.B) {
		db := New()
		defer db.Close()
		u := benchUser(b, db)
		frame := signedFrame(b, db, u, db.state.Next())
		p := benchPeer(b, db, false)
		msgs, err := readFrame([]byte(strings.Replace(frame, `"ID"`, `"first"`, 1)))
		if err != nil {
			b.Fatal(err)
		}
		db.handle(p, &msgs[0])
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			i++
			msgs, err := readFrame([]byte(strings.Replace(frame, `"ID"`, `"k`+strconv.Itoa(i)+`"`, 1)))
			if err != nil {
				b.Fatal(err)
			}
			db.handle(p, &msgs[0])
		}
	})
}

// BenchmarkOnceFieldLargeNode reads one field of a node with n fields
// through a path (optimization 4).
func BenchmarkOnceFieldLargeNode(b *testing.B) {
	for _, n := range []int{10, 10000} {
		b.Run("fields="+strconv.Itoa(n), func(b *testing.B) {
			db := New()
			defer db.Close()
			ctx := context.Background()
			fields := make(map[string]any, n)
			for i := range n {
				fields["k"+strconv.Itoa(i)] = map[string]any{"v": i}
			}
			if err := db.Get("list").Put(ctx, fields); err != nil {
				b.Fatal(err)
			}
			ref := db.Get("list").Get("k7").Get("v")
			b.ReportAllocs()
			for b.Loop() {
				if v, err := ref.Once[int](ctx); err != nil || v != 7 {
					b.Fatal(v, err)
				}
			}
		})
	}
}

// BenchmarkOnPutNested changes one field deep in a document of n linked
// nodes while On watches the document (optimization 5).
func BenchmarkOnPutNested(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run("nodes="+strconv.Itoa(n), func(b *testing.B) {
			db := New()
			defer db.Close()
			ctx := context.Background()
			doc := make(map[string]any, n)
			for i := range n {
				doc["c"+strconv.Itoa(i)] = map[string]any{"v": -1, "name": "child " + strconv.Itoa(i)}
			}
			root := db.Get("doc")
			if err := root.Put(ctx, doc); err != nil {
				b.Fatal(err)
			}
			type child struct {
				V int `json:"v"`
			}
			got := make(chan int, 1)
			off := root.On(func(m map[string]child) { got <- m["c0"].V })
			defer off()
			<-got
			ref := root.Get("c0").Get("v")
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				i++
				if err := ref.Put(ctx, i); err != nil {
					b.Fatal(err)
				}
				for v := <-got; v != i; v = <-got {
				}
			}
		})
	}
}

// BenchmarkGetMiss is a relay with 1,000 connected clients answering a get
// for a node it does not have (optimization 6).
func BenchmarkGetMiss(b *testing.B) { benchGetMiss(b, Options{}) }

// BenchmarkGetMissRouted is BenchmarkGetMiss with RouteByInterest.
func BenchmarkGetMissRouted(b *testing.B) { benchGetMiss(b, Options{RouteByInterest: true}) }

func benchGetMiss(b *testing.B, o Options) {
	db := New(o)
	defer db.Close()
	for range 1000 {
		benchPeer(b, db, true)
	}
	asker := benchPeer(b, db, true)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		msgs, err := readFrame([]byte(`{"#":"g` + strconv.Itoa(i) + `","get":{"#":"missing/` + strconv.Itoa(i) + `"}}`))
		if err != nil {
			b.Fatal(err)
		}
		db.handle(asker, &msgs[0])
	}
}

// BenchmarkPutLocalManyPeers is a server with 1,000 connected clients that
// writes a node none of them asked for (optimizations 6 and 10).
func BenchmarkPutLocalManyPeers(b *testing.B) { benchPutLocalManyPeers(b, Options{}) }

// BenchmarkPutLocalManyPeersRouted is BenchmarkPutLocalManyPeers with
// RouteByInterest.
func BenchmarkPutLocalManyPeersRouted(b *testing.B) {
	benchPutLocalManyPeers(b, Options{RouteByInterest: true})
}

func benchPutLocalManyPeers(b *testing.B, o Options) {
	db := New(o)
	defer db.Close()
	for range 1000 {
		benchPeer(b, db, true)
	}
	ctx := context.Background()
	ref := db.Get("server").Get("stats")
	if err := ref.Put(ctx, map[string]any{"n": 0}); err != nil { // asks the peers once
		b.Fatal(err)
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		if err := ref.Put(ctx, map[string]any{"n": i}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkResubscribe is a client that has read n souls connecting to a
// peer: it asks for them again and offers its copies (optimization 7).
func BenchmarkResubscribe(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		b.Run("souls="+strconv.Itoa(n), func(b *testing.B) {
			db := New()
			defer db.Close()
			ctx := context.Background()
			for i := range n {
				soul := "item/" + strconv.Itoa(i)
				if err := db.Get(soul).Put(ctx, map[string]any{"title": "item " + strconv.Itoa(i), "done": false}); err != nil {
					b.Fatal(err)
				}
				done := make(chan struct{})
				close(done)
				db.interest[interestKey{soul: soul}] = &interest{done: done}
			}
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				p := benchPeer(b, db, false)
				b.StartTimer()
				db.resubscribe(p)
				b.StopTimer()
				p.close()
				b.StartTimer()
			}
		})
	}
}

// BenchmarkSoulsRange lists 100 souls out of 100,000 with one prefix
// (optimization 8).
func BenchmarkSoulsRange(b *testing.B) {
	db := New()
	defer db.Close()
	ctx := context.Background()
	for i := range 100000 {
		if err := db.Get(fmt.Sprintf("chat/%06d", i)).Put(ctx, map[string]any{"text": "hi"}); err != nil {
			b.Fatal(err)
		}
	}
	q := Lex{Prefix: "chat/", From: "chat/050000", To: "chat/050099"}
	b.ReportAllocs()
	for b.Loop() {
		souls, err := db.Souls(ctx, q)
		if err != nil || len(souls) != 100 {
			b.Fatal(len(souls), err)
		}
	}
}

// BenchmarkHandlePutLargeNode is a relay passing on a put of one node with
// 1,000 fields to one subscriber (optimization 9).
func BenchmarkHandlePutLargeNode(b *testing.B) {
	db := New()
	defer db.Close()
	sub := benchPeer(b, db, false)
	sub.want("list")
	writer := benchPeer(b, db, false)
	var sb strings.Builder
	sb.WriteString(`{"#":"ID","put":{"list":{"_":{"#":"list",">":{`)
	for i := range 1000 {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `"item-%d":STATE`, i)
	}
	sb.WriteString(`}}`)
	for i := range 1000 {
		fmt.Fprintf(&sb, `,"item-%d":{"#":"item-%d"}`, i, i)
	}
	sb.WriteString(`}}}`)
	frame := sb.String()
	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		f := strings.ReplaceAll(frame, "STATE", strconv.Itoa(1700000000000+i))
		f = strings.Replace(f, `"ID"`, `"m`+strconv.Itoa(i)+`"`, 1)
		msgs, err := readFrame([]byte(f))
		if err != nil {
			b.Fatal(err)
		}
		db.handle(writer, &msgs[0])
	}
}

// BenchmarkDupTrackParallel tracks message IDs from every core at once, as
// the read loops of many peers do (optimization 11).
func BenchmarkDupTrackParallel(b *testing.B) {
	d := newDup(0)
	ids := make([]string, 1<<16)
	for i := range ids {
		ids[i] = randomID(9)
	}
	var next atomic.Int64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := int(next.Add(1)) * 7919
		for pb.Next() {
			d.track(ids[i%len(ids)], nil)
			i++
		}
	})
}

// sizeConn records the size of the largest frame sent.
type sizeConn struct {
	fakeConn
	max   atomic.Int64
	total atomic.Int64
}

func (c *sizeConn) Send(_ context.Context, frame []byte) error {
	n := int64(len(frame))
	c.total.Add(n)
	for {
		m := c.max.Load()
		if n <= m || c.max.CompareAndSwap(m, n) {
			return nil
		}
	}
}

// BenchmarkGetLargeNode is a relay answering a get for a whole node of
// 100,000 fields; max-frame-B is the largest frame it sends
// (optimization 13).
func BenchmarkGetLargeNode(b *testing.B) {
	db := New()
	defer db.Close()
	fields := make(map[string]any, 100000)
	for i := range 100000 {
		fields["item-"+strconv.Itoa(i)] = i
	}
	if err := db.Get("list").Put(context.Background(), fields); err != nil {
		b.Fatal(err)
	}
	conn := &sizeConn{fakeConn: *newFakeConn(false)}
	ctx, cancel := context.WithCancel(db.ctx)
	defer cancel()
	p := &peer{db: db, conn: conn, ctx: ctx, cancel: cancel, outWake: make(chan struct{}, 1), wants: map[string]bool{}}
	go p.writeLoop()
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		msgs, err := readFrame([]byte(`{"#":"g` + strconv.Itoa(i) + `","get":{"#":"list"}}`))
		if err != nil {
			b.Fatal(err)
		}
		db.handle(p, &msgs[0])
	}
	for conn.max.Load() == 0 { // the write loop sends it
		time.Sleep(time.Millisecond)
	}
	b.ReportMetric(float64(conn.max.Load()), "max-frame-B")
}

// BenchmarkFrameDecodeGet decodes a batch of ten gets, the other half of
// what clients send (optimization 9).
func BenchmarkFrameDecodeGet(b *testing.B) {
	parts := make([]string, 10)
	for i := range parts {
		parts[i] = `{"#":"g` + strconv.Itoa(i) + `","get":{"#":"users/alice",".":"name"}}`
	}
	frame := []byte("[" + strings.Join(parts, ",") + "]")
	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := readFrame(frame); err != nil {
			b.Fatal(err)
		}
	}
}
