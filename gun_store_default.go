package gundb

import (
	"context"
	"sync"
)

// NewMemoryStore returns the in-memory Store that New uses by default.
func NewMemoryStore() Store { return &memoryStore{nodes: map[string]*Node{}} }

type memoryStore struct {
	mu    sync.RWMutex
	nodes map[string]*Node
}

func (s *memoryStore) Get(_ context.Context, soul string) (*Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if n, ok := s.nodes[soul]; ok {
		return n.Clone(), nil
	}
	return nil, nil
}

func (s *memoryStore) Put(_ context.Context, n *Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[n.Soul] = n.Clone()
	return nil
}
