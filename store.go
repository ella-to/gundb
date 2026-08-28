package gundb

import "context"

// Store persists nodes. The runtime does all HAM merging; a Store is a plain
// soul -> node map.
//
//   - Get returns (nil, nil) when the soul is unknown.
//   - Get must return a node the caller may modify (a copy).
//   - Put must not keep a reference to the node it is given.
//   - Both are called concurrently.
//
// The default is an in-memory store; see examples/persist for a file-backed one.
type Store interface {
	Get(ctx context.Context, soul string) (*Node, error)
	Put(ctx context.Context, node *Node) error
}
