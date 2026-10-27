package gundb

import (
	"context"
	"strconv"
	"strings"
	"testing"
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
