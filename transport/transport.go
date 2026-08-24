// Package transport defines the wire-level interface between gundb peers.
//
// A Transport is responsible only for moving raw byte frames between two
// endpoints; framing, encoding and protocol semantics live in package gundb.
// This separation lets WebSocket, WebRTC, in-process pipes, libp2p streams
// or anything else plug in without touching the database core.
package transport

import (
	"context"
	"errors"
	"io"
)

// Conn is a single bidirectional connection to a remote peer. Both directions
// are byte-frame oriented (each Send / Recv is one logical message), matching
// how WebSocket and similar transports already behave.
//
// Implementations MUST be safe for concurrent Send from multiple goroutines
// and for one reader goroutine calling Recv.
type Conn interface {
	// Send writes a single frame. The slice is owned by the caller; Send
	// must not retain a reference past the call.
	Send(ctx context.Context, frame []byte) error

	// Recv returns the next frame. It blocks until a frame is available or
	// the context is cancelled. On normal close it returns io.EOF.
	Recv(ctx context.Context) ([]byte, error)

	// Close terminates the connection. Calling Close more than once is
	// allowed; subsequent calls return ErrClosed.
	Close() error

	// RemoteAddr returns a human-readable identifier for the remote peer
	// (URL, network address, etc.). It is used for logging only.
	RemoteAddr() string
}

// Dialer establishes a Conn to a remote address (URL for WebSocket, etc.).
type Dialer interface {
	Dial(ctx context.Context, addr string) (Conn, error)
}

// Listener accepts incoming connections from remote peers. Each call to
// Accept returns one new Conn; the listener does not auto-spawn goroutines.
type Listener interface {
	// Accept blocks until a peer connects, or until Close is called (in
	// which case it returns ErrClosed).
	Accept(ctx context.Context) (Conn, error)
	// Close stops accepting and frees the underlying resources.
	Close() error
	// Addr returns the address the listener is bound to.
	Addr() string
}

// ErrClosed is returned by Send / Recv / Accept after the underlying
// connection or listener has been closed.
var ErrClosed = errors.New("transport: closed")

// IsClosed reports whether err signals a closed transport (ErrClosed or EOF).
func IsClosed(err error) bool {
	return errors.Is(err, ErrClosed) || errors.Is(err, io.EOF)
}
