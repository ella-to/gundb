package gundb

import (
	"context"
	"iter"
	"strings"
	"sync"

	"ella.to/gundb/internal/soulindex"
)

// NewMemoryStore returns the in-memory Store that New uses by default.
func NewMemoryStore() Store { return &memoryStore{nodes: map[string]*Node{}} }

var (
	_ FieldReader    = (*memoryStore)(nil)
	_ LiveReader     = (*memoryStore)(nil)
	_ SoulRangeStore = (*memoryStore)(nil)
	_ ScanStore      = (*memoryStore)(nil)
)

type memoryStore struct {
	mu    sync.RWMutex
	nodes map[string]*Node

	idxMu sync.Mutex // taken inside mu, never the other way round
	idx   soulindex.Index
}

// added records a new soul in the index. Callers hold mu for writing.
func (s *memoryStore) added(soul string) {
	s.idxMu.Lock()
	s.idx.Add(soul)
	s.idxMu.Unlock()
}

func (s *memoryStore) SoulsBetween(_ context.Context, prefix, from, to string) ([]string, error) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	return s.idx.Between(prefix, from, to), nil
}

func (s *memoryStore) Scan(context.Context) iter.Seq2[*Node, error] {
	return func(yield func(*Node, error) bool) {
		s.mu.RLock()
		nodes := make([]*Node, 0, len(s.nodes))
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
	if _, ok := s.nodes[n.Soul]; !ok {
		s.added(n.Soul)
	}
	s.nodes[n.Soul] = n.Clone()
	return nil
}

func (s *memoryStore) PutFields(_ context.Context, soul string, changed *Node) error {
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

// getField reads one field without copying the node, for reader.field.
func (s *memoryStore) getField(soul, field string) Value {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if n := s.nodes[soul]; n != nil {
		return n.Fields[field]
	}
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

// HasLive implements LiveReader without copying the node.
func (s *memoryStore) HasLive(_ context.Context, soul string, skip []string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[soul]
	return ok && HasLiveField(n, skip), nil
}
