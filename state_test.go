package gundb

import "testing"

func TestStateIsStrictlyIncreasing(t *testing.T) {
	now := int64(1000)
	s := &stateGen{nowMs: func() int64 { return now }}
	a, b, c := s.Next(), s.Next(), s.Next()
	if !(a < b && b < c) || a != 1000 || b != 1000+1.0/999 {
		t.Fatalf("got %v %v %v", a, b, c)
	}
	now++
	if d := s.Next(); d != 1001 {
		t.Fatalf("new millisecond should reset the counter, got %v", d)
	}
}
