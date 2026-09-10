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

// FieldStore is an optional extension of Store. When the store implements
// it, the runtime calls PutFields with only the fields that won HAM in a
// merge, instead of Put with the whole merged node. Stores that keep history
// or write field by field use it to avoid rewriting unchanged fields.
//
// changed holds the accepted fields of soul; it must not be retained.
type FieldStore interface {
	Store
	PutFields(ctx context.Context, soul string, changed *Node) error
}
