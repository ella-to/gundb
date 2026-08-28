// Package memstore is an in-memory gundb.Store that can list its souls.
//
// gundb.New already uses an in-memory store by default; use this one when
// you want to inspect what a peer holds, e.g. in tests or a debug endpoint.
package memstore

import (
	"context"
	"maps"
	"slices"
	"sync"

	"ella.to/gundb"
)

// Store is an in-memory gundb.Store.
type Store struct {
	mu    sync.RWMutex
	nodes map[string]*gundb.Node
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
	s.nodes[n.Soul] = n.Clone()
	return nil
}

// Souls returns the stored souls, sorted.
func (s *Store) Souls() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Sorted(maps.Keys(s.nodes))
}
