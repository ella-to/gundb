package gundb

import (
	"sync"
	"time"
)

// stateGen produces HAM states exactly like gun/src/state.js: wall-clock
// milliseconds, plus N/999 when called several times within the same
// millisecond, so states are strictly increasing.
type stateGen struct {
	mu    sync.Mutex
	last  float64
	n     int
	nowMs func() int64
}

func newStateGen() *stateGen {
	return &stateGen{nowMs: func() int64 { return time.Now().UnixMilli() }}
}

func (s *stateGen) Next() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := float64(s.nowMs())
	if s.last < t {
		s.n = 0
		s.last = t
		return t
	}
	s.n++
	s.last = t + float64(s.n)/999
	return s.last
}
