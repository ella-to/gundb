package gundb

import (
	"context"
	"strings"
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

func (s *memoryStore) ListSouls(_ context.Context, prefix string) ([]string, error) {
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
