package gundb

import (
	"context"
	"errors"
	"sync"
)

// ErrOtherDB is returned when a Batch is given a ref from another DB.
var ErrOtherDB = errors.New("gundb: ref belongs to another DB")

// Batch collects several writes and sends them as one, so they land
// together or not at all:
//
//	b := db.Batch()
//	b.Set(ctx, db.Get("a").Get("following"), db.Get("b"))
//	b.Set(ctx, db.Get("b").Get("followers"), db.Get("a"))
//	err := b.Commit(ctx)
//
// Nothing is written before Commit. Commit applies every write locally in
// one step and sends them to peers as one message. A peer checks the whole
// message first (signatures, Options.CanWrite) and applies all of it or
// none of it. With a BatchStore the local commit is one PutBatch; another
// store saves node by node, so a store error can leave part of it saved.
//
// It is not a database transaction. There is no rollback: once sent, a
// write is on other peers, and undoing it is just another write. Each field
// still merges on its own (HAM), so a newer write from elsewhere can win
// over part of a committed batch. And like Put, a batch is applied locally
// before peers answer: if CommitAck reports ErrForbidden, the local copy
// still holds the writes.
//
// Refs signed by different users can be mixed: each user signs the nodes
// in their own space. A Batch is safe for concurrent use.
type Batch struct {
	db  *DB
	mu  sync.Mutex
	g   graph
	by  map[string]*User // signers, by public key
	err error            // first failed write; Commit returns it
}

// Batch starts an empty batch of writes.
func (db *DB) Batch() *Batch { return &Batch{db: db, g: graph{}, by: map[string]*User{}} }

// Put adds r.Put(ctx, v) to the batch. ctx is used to look up the souls of
// r's path; the write itself waits for Commit.
func (b *Batch) Put(ctx context.Context, r *Ref, v any) error {
	return b.add(r, func(g graph, state float64) error { return r.addTo(ctx, g, v, state) })
}

// Set adds r.Set(ctx, v) to the batch and returns a ref to the new item.
// The item exists once Commit succeeds.
func (b *Batch) Set(ctx context.Context, r *Ref, v any) (*Ref, error) {
	var item *Ref
	err := b.add(r, func(g graph, state float64) (err error) {
		item, err = r.setTo(ctx, g, v, state)
		return err
	})
	if err != nil {
		return nil, err
	}
	return item, nil
}

// Len returns the number of nodes the batch writes.
func (b *Batch) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.g)
}

// Commit writes the batch and sends it to every peer, without waiting for
// them. If an earlier Put or Set failed, it writes nothing and returns that
// error. Either way the batch is empty afterwards and can be reused.
func (b *Batch) Commit(ctx context.Context) error { return b.commit(ctx, false) }

// CommitAck is Commit that also waits until at least one peer acknowledges
// the batch, like PutAck. It returns ErrNoPeers if there is nobody to ask;
// the batch is still saved locally and sent when a peer connects.
func (b *Batch) CommitAck(ctx context.Context) error { return b.commit(ctx, true) }

// add builds one write into its own graph, then merges it into the batch.
// Building can read from peers, so it runs without the lock.
func (b *Batch) add(r *Ref, build func(g graph, state float64) error) error {
	if r.db != b.db {
		return b.fail(ErrOtherDB)
	}
	part := graph{}
	if err := build(part, b.db.state.Next()); err != nil {
		return b.fail(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.user != nil {
		b.by[r.user.Pub()] = r.user
	}
	for soul, n := range part {
		dst := b.g.node(soul)
		for k, v := range n.Fields {
			// Writes to the same field: the later one wins, as it would
			// if each were written on its own.
			if s, ok := dst.States[k]; !ok || n.States[k] >= s {
				dst.Set(k, v, n.States[k])
			}
		}
	}
	return nil
}

func (b *Batch) fail(err error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err == nil {
		b.err = err
	}
	return err
}

func (b *Batch) commit(ctx context.Context, ack bool) error {
	b.mu.Lock()
	g, by, err := b.g, b.by, b.err
	b.g, b.by, b.err = graph{}, map[string]*User{}, nil
	b.mu.Unlock()
	if err != nil || len(g) == 0 {
		return err
	}
	var signed []checked
	for _, u := range by {
		c, err := u.sign(g)
		if err != nil {
			return err
		}
		signed = append(signed, c)
	}
	var skip checked
	if len(signed) > 0 {
		skip = func(soul, field string) bool {
			for _, c := range signed {
				if c(soul, field) {
					return true
				}
			}
			return false
		}
	}
	return b.db.write(ctx, g, ack, skip)
}
