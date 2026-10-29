// Package memstore is an in-memory gundb.Store that can list its souls.
//
// gundb.New already uses an in-memory store by default; use this one when
// you want to inspect what a peer holds, e.g. in tests or a debug endpoint.
package memstore

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"

	"ella.to/gundb"
)

// Store is an in-memory gundb.SoulStore and gundb.FieldReader.
type Store struct {
	mu    sync.RWMutex
	nodes map[string]*gundb.Node
}

var (
	_ gundb.SoulStore   = (*Store)(nil)
	_ gundb.FieldReader = (*Store)(nil)
)

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

// PutFields implements gundb.FieldStore: only the changed fields are written.
func (s *Store) PutFields(_ context.Context, soul string, changed *gundb.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[soul]
	if !ok {
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
