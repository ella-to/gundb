// Package memstore is an in-memory gundb.Store that can list its souls.
//
// gundb.New already uses an in-memory store by default; use this one when
// you want to inspect what a peer holds, e.g. in tests or a debug endpoint.
package memstore

import (
	"context"
	"iter"
	"maps"
	"slices"
	"strings"
	"sync"

	"ella.to/gundb"
	"ella.to/gundb/internal/soulindex"
)

// Store is an in-memory gundb.SoulRangeStore and gundb.FieldReader.
type Store struct {
	mu    sync.RWMutex
	nodes map[string]*gundb.Node

	idxMu sync.Mutex // taken inside mu, never the other way round
	idx   soulindex.Index
}

var (
	_ gundb.SoulRangeStore = (*Store)(nil)
	_ gundb.FieldReader    = (*Store)(nil)
	_ gundb.LiveReader     = (*Store)(nil)
	_ gundb.ScanStore      = (*Store)(nil)
)

// added records a new soul in the index. Callers hold mu for writing.
func (s *Store) added(soul string) {
	s.idxMu.Lock()
	s.idx.Add(soul)
	s.idxMu.Unlock()
}

// SoulsBetween implements gundb.SoulRangeStore.
func (s *Store) SoulsBetween(_ context.Context, prefix, from, to string) ([]string, error) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	return s.idx.Between(prefix, from, to), nil
}

// Scan implements gundb.ScanStore.
func (s *Store) Scan(context.Context) iter.Seq2[*gundb.Node, error] {
	return func(yield func(*gundb.Node, error) bool) {
		s.mu.RLock()
		nodes := make([]*gundb.Node, 0, len(s.nodes))
		for _, n := range s.nodes {
			nodes = append(nodes, n.Clone())
		}
		s.mu.RUnlock()
		for _, n := range nodes {
			if !yield(n, nil) {
				return
			}
		}
	}
}

// New returns an empty Store.
func New() *Store { return &Store{nodes: map[string]*gundb.Node{}} }

// Get implements gundb.Store.
func (s *Store) Get(_ context.Context, soul string) (*gundb.Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if n, ok := s.nodes[soul]; ok {
		return n.Clone(), nil
	}
	return nil, nil
}

// Put implements gundb.Store.
func (s *Store) Put(_ context.Context, n *gundb.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.nodes[n.Soul]; !ok {
		s.added(n.Soul)
	}
	s.nodes[n.Soul] = n.Clone()
	return nil
}

// PutFields implements gundb.FieldStore: only the changed fields are written.
func (s *Store) PutFields(_ context.Context, soul string, changed *gundb.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[soul]
	if !ok {
		s.added(soul)
		s.nodes[soul] = changed.Clone()
		return nil
	}
	for k, v := range changed.Fields {
		n.Set(k, v, changed.States[k])
	}
	return nil
}

// GetFields implements gundb.FieldReader.
func (s *Store) GetFields(_ context.Context, soul string, fields []string) (*gundb.Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stored, ok := s.nodes[soul]
	if !ok {
		return nil, nil
	}
	var n *gundb.Node
	for _, k := range fields {
		if v, ok := stored.Fields[k]; ok {
			if n == nil {
				n = gundb.NewNode(soul)
			}
			n.Set(k, v, stored.States[k])
		}
	}
	return n, nil
}

// HasLive implements gundb.LiveReader without copying the node.
func (s *Store) HasLive(_ context.Context, soul string, skip []string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[soul]
	return ok && gundb.HasLiveField(n, skip), nil
}

// Souls returns the stored souls, sorted.
func (s *Store) Souls() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Sorted(maps.Keys(s.nodes))
}

// ListSouls implements gundb.SoulStore.
func (s *Store) ListSouls(_ context.Context, prefix string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var souls []string
	for soul := range s.nodes {
		if strings.HasPrefix(soul, prefix) {
			souls = append(souls, soul)
		}
	}
	return souls, nil
}
