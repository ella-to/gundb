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

// FieldReader is an optional extension of FieldStore. When the store
// implements it, a merge reads only the fields it is about to write,
// instead of the whole node, so writing one field of a large node (adding
// an item to a long list) costs the same as writing one field of a small
// one.
//
// GetFields returns a node holding those of fields that are stored, or
// (nil, nil) if there are none. Like Get, the node is the caller's to
// modify.
type FieldReader interface {
	FieldStore
	GetFields(ctx context.Context, soul string, fields []string) (*Node, error)
}

// BatchStore is an optional extension of FieldStore. When one write
// touches several nodes (a Set, a nested struct, an account), the runtime
// calls PutBatch once with the changed fields of every node, instead of
// PutFields per node, so the store can commit them together: one disk sync
// instead of one per node, and no reader sees half of the write.
//
// Each node in changed holds only the fields that won HAM; none may be
// retained.
type BatchStore interface {
	FieldStore
	PutBatch(ctx context.Context, changed []*Node) error
}

// SyncStore is an optional extension of BatchStore for stores that make
// writes durable with a sync, such as an fsync of a write-ahead log.
//
// The DB writes with PutBatchUnsynced while it holds the merge locks of
// the nodes, and calls Sync once it has let go of them. Writers to one
// node then wait for each other only while their write goes to memory,
// and their syncs can be shared. The puts of one frame from a peer share
// a single Sync, and are acknowledged once it returns, so an ack still
// means the write is durable.
type SyncStore interface {
	BatchStore
	// PutBatchUnsynced is PutBatch, without waiting for the write to be
	// durable: readers see it at once, and it is durable once a later Sync
	// returns.
	PutBatchUnsynced(ctx context.Context, changed []*Node) error
	// Sync returns once every write made before it is durable.
	Sync(ctx context.Context) error
}

// SoulStore is an optional extension of Store that lists souls. A DB needs
// it to answer LEX queries over souls, like {"#": {"*": "chat/"}} from GUN.js
// or DB.Souls; without it such gets are passed on to other peers.
//
// ListSouls returns every stored soul that starts with prefix, in any order.
type SoulStore interface {
	Store
	ListSouls(ctx context.Context, prefix string) ([]string, error)
}
