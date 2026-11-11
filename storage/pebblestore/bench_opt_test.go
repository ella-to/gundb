package pebblestore

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"ella.to/gundb"
	"ella.to/gundb/transport"
)

// Benchmarks for the optimizations that involve Pebble.

// flushedStore is benchStore with everything flushed to tables, as on a
// relay that has been running for a while.
func flushedStore(b *testing.B, nodes int, opts ...Options) *Store {
	b.Helper()
	o := Options{NoSync: true}
	if len(opts) > 0 {
		o = opts[0]
	}
	s, err := Open(b.TempDir(), o)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	for i := range nodes {
		n := gundb.NewNode(fmt.Sprintf("users/user%06d", i))
		for f := range 5 {
			n.Set(fmt.Sprintf("field%d", f), gundb.String(fmt.Sprintf("value %d of user %d", f, i)), 1)
		}
		if err := s.Put(b.Context(), n); err != nil {
			b.Fatal(err)
		}
	}
	if err := s.db.Flush(); err != nil {
		b.Fatal(err)
	}
	return s
}

// BenchmarkGetFieldsMissing looks up a field that no node has yet, which is
// what every merge of a new field does (optimization 3).
func BenchmarkGetFieldsMissing(b *testing.B) {
	const nodes = 100000
	s := flushedStore(b, nodes)
	r := rand.New(rand.NewPCG(3, 3))
	b.ReportAllocs()
	for b.Loop() {
		soul := fmt.Sprintf("users/user%06d", r.IntN(nodes))
		if n, err := s.GetFields(b.Context(), soul, []string{"missing"}); err != nil || n != nil {
			b.Fatal(n, err)
		}
	}
}

// BenchmarkPutNewField adds a new field to a random node through a DB
// (optimization 3).
func BenchmarkPutNewField(b *testing.B) {
	const nodes = 100000
	s := flushedStore(b, nodes)
	db := gundb.New(gundb.Options{Store: s})
	defer db.Close()
	ctx := context.Background()
	r := rand.New(rand.NewPCG(4, 4))
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		ref := db.Get(fmt.Sprintf("users/user%06d", r.IntN(nodes))).Get("new" + strconv.Itoa(i))
		if err := ref.Put(ctx, i); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHotSoulSynced writes to one node from many goroutines on a
// store that syncs every write, like a busy chat room (optimization 2).
func BenchmarkHotSoulSynced(b *testing.B) {
	s, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	db := gundb.New(gundb.Options{Store: s})
	defer db.Close()
	ctx := context.Background()
	room := db.Get("room")
	var n atomic.Int64
	b.SetParallelism(4)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := n.Add(1)
			if err := room.Get("m"+strconv.FormatInt(i, 10)).Put(ctx, "hello"); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

// frameConn feeds frames to a DB and counts the acks it sends back.
type frameConn struct {
	in     chan []byte
	acks   chan int
	closed chan struct{}
	once   sync.Once
}

func newFrameConn() *frameConn {
	return &frameConn{in: make(chan []byte), acks: make(chan int, 1024), closed: make(chan struct{})}
}

func (c *frameConn) Send(_ context.Context, frame []byte) error {
	if n := bytes.Count(frame, []byte(`"ok":{"":1}`)); n > 0 {
		c.acks <- n
	}
	return nil
}

func (c *frameConn) Recv(ctx context.Context) ([]byte, error) {
	select {
	case f := <-c.in:
		return f, nil
	case <-c.closed:
	case <-ctx.Done():
	}
	return nil, transport.ErrClosed
}

func (c *frameConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *frameConn) RemoteAddr() string { return "frames" }

// BenchmarkFrameOfPutsSynced is one peer sending a frame of 100 puts, each
// to its own node, to a DB on a store that syncs every write, and waiting
// for the 100 acks (optimization 2).
func BenchmarkFrameOfPutsSynced(b *testing.B) {
	s, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	db := gundb.New(gundb.Options{Store: s})
	defer db.Close()
	conn := newFrameConn()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go db.Attach(ctx, conn)
	defer conn.Close()
	b.ReportAllocs()
	round := 0
	for b.Loop() {
		round++
		var sb strings.Builder
		sb.WriteByte('[')
		for i := range 100 {
			if i > 0 {
				sb.WriteByte(',')
			}
			soul := fmt.Sprintf("n%d", i)
			fmt.Fprintf(&sb, `{"#":"r%dm%d","put":{%q:{"_":{"#":%q,">":{"v":%d}},"v":%d}}}`, round, i, soul, soul, 1700000000000+round, round)
		}
		sb.WriteByte(']')
		conn.in <- []byte(sb.String())
		for got := 0; got < 100; {
			got += <-conn.acks
		}
	}
}

// BenchmarkOnceFieldLargeNodePebble reads one field of a node with n
// fields through a path (optimization 4).
func BenchmarkOnceFieldLargeNodePebble(b *testing.B) {
	for _, n := range []int{10, 10000} {
		b.Run(fmt.Sprintf("fields=%d", n), func(b *testing.B) {
			s := flushedStore(b, 0)
			db := gundb.New(gundb.Options{Store: s})
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

// BenchmarkSoulsRangePebble lists 100 souls out of 100,000 with one
// prefix (optimization 8).
func BenchmarkSoulsRangePebble(b *testing.B) {
	s := flushedStore(b, 100000)
	db := gundb.New(gundb.Options{Store: s})
	defer db.Close()
	ctx := context.Background()
	q := gundb.Lex{Prefix: "users/", From: "users/user050000", To: "users/user050099"}
	b.ReportAllocs()
	for b.Loop() {
		souls, err := db.Souls(ctx, q)
		if err != nil || len(souls) != 100 {
			b.Fatal(len(souls), err)
		}
	}
}

// BenchmarkQuotaLoad opens a DB with a quota on a store of 100,000 nodes,
// which counts everything stored (optimization 8).
func BenchmarkQuotaLoad(b *testing.B) {
	s := flushedStore(b, 100000)
	quota := &gundb.Quota{Owner: func(soul string) string { return soul[:len("users/user00")] }}
	b.ReportAllocs()
	for b.Loop() {
		db := gundb.New(gundb.Options{Store: s, Quota: quota})
		if u := db.Usage("users/user00"); u.Nodes != 10000 {
			b.Fatal(u)
		}
		db.Close()
	}
}
