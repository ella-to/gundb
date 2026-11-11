package ws_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"ella.to/gundb/transport"
	"ella.to/gundb/transport/ws"
)

// countingListener counts the bytes the server reads off the network.
type countingListener struct {
	net.Listener
	read *atomic.Int64
}

func (l countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return countingConn{c, l.read}, nil
}

type countingConn struct {
	net.Conn
	read *atomic.Int64
}

func (c countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

// gunFrame is a batch of ten puts, like a busy client sends.
func gunFrame() []byte {
	parts := make([]string, 10)
	for i := range parts {
		id := strconv.Itoa(i)
		parts[i] = `{"#":"J0XPPtC` + id + `","put":{"users/alice` + id + `":{"_":{"#":"users/alice` + id + `",">":{"name":1700000000000,"age":1700000000000,"bio":1700000000000,"boss":1700000000000}},"name":"Alice","age":30,"bio":"Writes Go & JS all day","boss":{"#":"users/bob"}}}}`
	}
	return []byte("[" + strings.Join(parts, ",") + "]")
}

// benchSendRecv sends frames from a dialled conn to an accepted one and
// reports the bytes that crossed the network per frame (optimization 12).
func benchSendRecv(b *testing.B, accept func(http.ResponseWriter, *http.Request) (transport.Conn, error), dialer ws.Dialer) {
	var read atomic.Int64
	conns := make(chan transport.Conn, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := accept(w, r)
		if err != nil {
			b.Error(err)
			return
		}
		conns <- c
		<-r.Context().Done()
	}))
	srv.Listener = countingListener{srv.Listener, &read}
	srv.Start()
	defer srv.Close()
	ctx := context.Background()
	client, err := dialer.Dial(ctx, srv.URL)
	if err != nil {
		b.Fatal(err)
	}
	defer client.Close()
	server := <-conns
	defer server.Close()
	frame := gunFrame()
	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	read.Store(0)
	n := 0
	for b.Loop() {
		n++
		if err := client.Send(ctx, frame); err != nil {
			b.Fatal(err)
		}
		if _, err := server.Recv(ctx); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(read.Load())/float64(n), "wire-B/op")
}

func BenchmarkSendRecv(b *testing.B) {
	b.Run("default", func(b *testing.B) { benchSendRecv(b, ws.Accept, ws.Dialer{}) })
}
