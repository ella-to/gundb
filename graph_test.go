package gundb

import (
	"encoding/json"
	"testing"
)

func TestNodeWireShape(t *testing.T) {
	n := NewNode("mark")
	n.Set("name", String("Mark"), 1791147867592)
	n.Set("boss", Link{Soul: "mark/boss"}, 1791147867592.002)
	b, err := marshalWire(n)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"_":{"#":"mark",">":{"boss":1791147867592.002,"name":1791147867592}},"boss":{"#":"mark/boss"},"name":"Mark"}`
	if string(b) != want {
		t.Fatalf("\n got %s\nwant %s", b, want)
	}
	var back Node
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Fields["boss"] != (Link{Soul: "mark/boss"}) || back.States["boss"] != 1791147867592.002 {
		t.Fatalf("round trip: %+v", back)
	}
}

func TestNodeRejectsInvalid(t *testing.T) {
	for _, s := range []string{
		`{"name":"x"}`,                          // no metadata
		`{"_":{">":{}},"name":"x"}`,             // no soul
		`{"_":{"#":"a",">":{}},"name":"x"}`,     // no state
		`{"_":{"#":"a",">":{"l":1}},"l":[1,2]}`, // array
	} {
		var n Node
		if err := json.Unmarshal([]byte(s), &n); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
}

func TestChildrenFirst(t *testing.T) {
	g := graph{}
	g.node("a").Set("b", Link{Soul: "b"}, 1)
	g.node("b").Set("c", Link{Soul: "c"}, 1)
	g.node("b").Set("a", Link{Soul: "a"}, 1) // a cycle must not loop
	g.node("c").Set("x", Link{Soul: "elsewhere"}, 1)
	for range 50 { // map order varies between runs
		pos := map[string]int{}
		for i, soul := range childrenFirst(g) {
			pos[soul] = i
		}
		if len(pos) != 3 || pos["c"] > pos["b"] {
			t.Fatalf("got order %v", pos)
		}
	}
}
