package gundb

import (
	"context"
	"strings"
	"sync"
)

// NewMemoryStore returns the in-memory Store that New uses by default.
func NewMemoryStore() Store { return &memoryStore{nodes: map[string]*Node{}} }

var _ FieldReader = (*memoryStore)(nil)

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

func (s *memoryStore) PutFields(_ context.Context, soul string, changed *Node) error {
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

func (s *memoryStore) GetFields(_ context.Context, soul string, fields []string) (*Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stored, ok := s.nodes[soul]
	if !ok {
		return nil, nil
	}
	var n *Node
	for _, k := range fields {
		if v, ok := stored.Fields[k]; ok {
			if n == nil {
				n = NewNode(soul)
			}
			n.Set(k, v, stored.States[k])
		}
	}
	return n, nil
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
