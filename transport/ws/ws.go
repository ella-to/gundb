// Package ws is the WebSocket adapter for gundb, wire-compatible with the
// WebSocket transport of the JavaScript GUN (browser and Node).
//
// You rarely need it directly: a *gundb.DB is an http.Handler that upgrades
// to WebSocket, and gundb dials ws://, wss://, http:// and https:// peers
// with this package by default.
//
//	http.Handle("/gun", db)                                      // server
//	db := gundb.New(gundb.Options{Peers: []string{"ws://host:8765/gun"}}) // client
//
// Use Accept, Dialer and Listen when you need control over the upgrade or
// the dial (custom origins, headers, TLS), then hand the connection to
// db.Attach.
package ws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"ella.to/gundb/transport"
)

// DefaultPath is the URL path GUN uses for its WebSocket endpoint.
const DefaultPath = "/gun"

// maxFrame caps the size of one incoming frame.
const maxFrame = 64 << 20

// Dialer connects to a GUN peer over WebSocket. The zero value is ready to use.
type Dialer struct {
	// Options is passed to websocket.Dial (headers, HTTP client, ...).
	Options *websocket.DialOptions
	// Compress offers to compress frames (permessage-deflate, each frame
	// on its own). The server decides; see AcceptOptions.Compress.
	Compress bool
}

// Dial connects to addr. http:// and https:// URLs are dialled as ws:// and
// wss://, like GUN's own peers option.
func (d Dialer) Dial(ctx context.Context, addr string) (transport.Conn, error) {
	url := addr
	switch {
	case strings.HasPrefix(url, "http://"):
		url = "ws://" + strings.TrimPrefix(url, "http://")
	case strings.HasPrefix(url, "https://"):
		url = "wss://" + strings.TrimPrefix(url, "https://")
	}
	opts := d.Options
	if d.Compress {
		o := websocket.DialOptions{}
		if opts != nil {
			o = *opts
		}
		o.CompressionMode = websocket.CompressionNoContextTakeover
		opts = &o
	}
	c, _, err := websocket.Dial(ctx, url, opts)
	if err != nil {
		return nil, fmt.Errorf("ws: dial %s: %w", addr, err)
	}
	c.SetReadLimit(maxFrame)
	return &Conn{ws: c, remote: addr}, nil
}

// Accept upgrades an HTTP request to a WebSocket connection. Any origin is
// allowed because GUN peers are cross-origin by design; call
// websocket.Accept yourself and wrap it with NewConn to restrict that.
func Accept(w http.ResponseWriter, r *http.Request) (transport.Conn, error) {
	return AcceptWith(w, r, AcceptOptions{})
}

// AcceptOptions tunes AcceptWith.
type AcceptOptions struct {
	// Compress compresses frames (permessage-deflate, each frame on its
	// own) with peers that offer it, as browsers and GUN.js on Node do.
	// GUN's JSON shrinks to a third or less, for some CPU per frame.
	Compress bool
}

// AcceptWith is Accept with options.
func AcceptWith(w http.ResponseWriter, r *http.Request, o AcceptOptions) (transport.Conn, error) {
	opts := &websocket.AcceptOptions{InsecureSkipVerify: true}
	if o.Compress {
		opts.CompressionMode = websocket.CompressionNoContextTakeover
	}
	c, err := websocket.Accept(w, r, opts)
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(maxFrame)
	return NewConn(c, r.RemoteAddr), nil
}

// NewConn wraps an established websocket connection.
func NewConn(c *websocket.Conn, remote string) *Conn { return &Conn{ws: c, remote: remote} }

// Conn is a transport.Conn over one WebSocket.
type Conn struct {
	ws     *websocket.Conn
	remote string
	mu     sync.Mutex // serialises writes
	once   sync.Once
}

// Send writes one text frame (GUN always speaks JSON text).
func (c *Conn) Send(ctx context.Context, frame []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ws.Write(ctx, websocket.MessageText, frame); err != nil {
		return closedOr(err)
	}
	return nil
}

// Recv reads the next frame. It is read into a buffer reused across
// frames, then copied once at its exact size, instead of growing a new
// buffer frame after frame.
func (c *Conn) Recv(ctx context.Context) ([]byte, error) {
	_, r, err := c.ws.Reader(ctx)
	if err != nil {
		return nil, closedOr(err)
	}
	buf := readBufs.Get().(*bytes.Buffer)
	defer func() {
		if buf.Cap() <= maxPooled {
			readBufs.Put(buf)
		}
	}()
	buf.Reset()
	if _, err := buf.ReadFrom(r); err != nil {
		return nil, closedOr(err)
	}
	return bytes.Clone(buf.Bytes()), nil
}

// readBufs are the buffers Recv reads into; one larger than maxPooled is
// let go, so one huge frame does not keep its memory.
var readBufs = sync.Pool{New: func() any { return new(bytes.Buffer) }}

const maxPooled = 1 << 20

// Ping sends a WebSocket ping and waits for the pong. It needs Recv to be
// running at the same time, which is how gundb uses a Conn.
func (c *Conn) Ping(ctx context.Context) error {
	if err := c.ws.Ping(ctx); err != nil {
		return closedOr(err)
	}
	return nil
}

// Close closes the connection with a normal closure.
func (c *Conn) Close() error {
	err := transport.ErrClosed
	c.once.Do(func() { err = c.ws.Close(websocket.StatusNormalClosure, "") })
	return err
}

// RemoteAddr returns the dialled URL or the client's network address.
func (c *Conn) RemoteAddr() string { return c.remote }

func closedOr(err error) error {
	if websocket.CloseStatus(err) != -1 || errors.Is(err, net.ErrClosed) {
		return transport.ErrClosed
	}
	return err
}

// Listener is a transport.Listener that serves WebSocket upgrades on one
// path of its own HTTP server. Prefer http.Handle("/gun", db) when you
// already run an HTTP server.
type Listener struct {
	addr   string
	server *http.Server
	conns  chan transport.Conn
	done   chan struct{}
	once   sync.Once
}

// Listen starts an HTTP server on addr that accepts WebSocket peers at path
// (DefaultPath if empty).
func Listen(addr, path string) (*Listener, error) {
	if path == "" {
		path = DefaultPath
	}
	nl, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ws: listen %s: %w", addr, err)
	}
	l := &Listener{addr: nl.Addr().String(), conns: make(chan transport.Conn), done: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(w, r)
		if err != nil {
			return
		}
		select {
		case l.conns <- c:
		case <-l.done:
			c.Close()
		}
	})
	l.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go l.server.Serve(nl)
	return l, nil
}

// Accept waits for the next peer.
func (l *Listener) Accept(ctx context.Context) (transport.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, transport.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close stops the HTTP server. Established connections stay open.
func (l *Listener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.server.Close()
}

// Addr returns the bound address, e.g. "127.0.0.1:8765".
func (l *Listener) Addr() string { return l.addr }
