package gundb

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"ella.to/gundb/transport"
)

// fakeConn records sent frames. With block set, Send waits until the
// connection is closed, like a peer that stopped reading.
type fakeConn struct {
	frames chan []byte
	block  bool
	closed chan struct{}
}

func newFakeConn(block bool) *fakeConn {
	return &fakeConn{frames: make(chan []byte, 1000), block: block, closed: make(chan struct{})}
}

func (c *fakeConn) Send(ctx context.Context, frame []byte) error {
	if c.block {
		select {
		case <-c.closed:
		case <-ctx.Done():
		}
		return transport.ErrClosed
	}
	c.frames <- append([]byte(nil), frame...)
	return nil
}

func (c *fakeConn) Recv(ctx context.Context) ([]byte, error) {
	select {
	case <-c.closed:
	case <-ctx.Done():
	}
	return nil, transport.ErrClosed
}

func (c *fakeConn) Close() error {
	select {
	case <-c.closed:
		return transport.ErrClosed
	default:
		close(c.closed)
		return nil
	}
}

func (c *fakeConn) RemoteAddr() string { return "fake" }

func newTestPeer(t *testing.T, conn transport.Conn) *peer {
	db := New()
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithCancel(db.ctx)
	t.Cleanup(cancel)
	return &peer{db: db, conn: conn, ctx: ctx, cancel: cancel, outWake: make(chan struct{}, 1), wants: map[string]bool{}}
}

func TestPeerQueueOrderAndBatches(t *testing.T) {
	conn := newFakeConn(false)
	p := newTestPeer(t, conn)
	const n = 250
	for i := range n {
		p.send([]byte(strconv.Itoa(i)))
	}
	if p.out == nil || len(p.out) != n {
		t.Fatalf("queued %d, want %d", len(p.out), n)
	}
	go p.writeLoop()

	var got []int
	for len(got) < n {
		frame := <-conn.frames
		var batch []int
		if frame[0] == '[' {
			if err := json.Unmarshal(frame, &batch); err != nil {
				t.Fatal(err)
			}
		} else {
			v, _ := strconv.Atoi(string(frame))
			batch = []int{v}
		}
		if len(batch) > maxBatch {
			t.Fatalf("frame with %d messages, max %d", len(batch), maxBatch)
		}
		got = append(got, batch...)
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("message %d is %d: out of order", i, v)
		}
	}
	p.outMu.Lock()
	defer p.outMu.Unlock()
	if p.out != nil {
		t.Fatalf("drained queue still holds %d slots", cap(p.out))
	}
}

func TestSlowPeerIsDisconnected(t *testing.T) {
	old := slowPeer
	slowPeer = 50 * time.Millisecond
	defer func() { slowPeer = old }()

	conn := newFakeConn(true)
	p := newTestPeer(t, conn)
	go p.writeLoop()
	for range maxOut + maxBatch + 1 { // more than fits, even after one batch is taken
		p.send([]byte("1"))
	}
	select {
	case <-p.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a peer that stopped reading was not disconnected")
	}
}
