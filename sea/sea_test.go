package sea

import (
	"encoding/json"
	"testing"
)

// Results of SEA.opt.pub in GUN.js for the same souls.
func TestPubOf(t *testing.T) {
	for _, c := range []struct{ soul, pub string }{
		{"~abc.def", "abc.def"},
		{"~abc.def/profile", "abc.def"},
		{"~abc.def/x/y", "abc.def"},
		{"~@alice", ".alice"},
		{"~@", "."},
		{"users", ""},
		{"~abc", ""},
		{"x~abc.def", "abc.def"},
		{"~abc.def~ghi.jkl", "abc.def"},
		{"~a_b-c.d-e_f/z", "a_b-c.d-e_f"},
		{"~abc.def#hash", "abc.def"},
		{"~.abc", ".abc"},
		{"~ab😀c.d", "ab."},
	} {
		if got := PubOf(c.soul); got != c.pub {
			t.Errorf("PubOf(%q) = %q, GUN.js says %q", c.soul, got, c.pub)
		}
	}
}

func TestFieldRoundTrip(t *testing.T) {
	pair, err := NewPair()
	if err != nil {
		t.Fatal(err)
	}
	soul := "~" + pair.Pub + "/profile"
	stored, err := SignField(soul, "name", json.RawMessage(`"Ali"`), 1700000000000.5, pair)
	if err != nil {
		t.Fatal(err)
	}
	v, err := VerifyField(soul, "name", stored, 1700000000000.5, pair.Pub)
	if err != nil || string(v) != `"Ali"` {
		t.Fatalf("VerifyField = %s, %v", v, err)
	}
	// The signature binds soul, field and state: moving it anywhere fails.
	for _, c := range []struct {
		soul, field string
		state       float64
	}{
		{soul + "x", "name", 1700000000000.5},
		{soul, "nick", 1700000000000.5},
		{soul, "name", 1700000000001},
	} {
		if _, err := VerifyField(c.soul, c.field, stored, c.state, pair.Pub); err != ErrSignature {
			t.Errorf("replayed to %+v: %v", c, err)
		}
	}
}
