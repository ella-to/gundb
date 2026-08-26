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
	c, _, err := websocket.Dial(ctx, url, d.Options)
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
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
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

// Recv reads the next frame.
func (c *Conn) Recv(ctx context.Context) ([]byte, error) {
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		return nil, closedOr(err)
	}
	return data, nil
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
