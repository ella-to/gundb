package gundb

import (
	"encoding/json"
	"strings"
	"testing"
)

// Frames captured from gun 0.2020.1241 (browser client <-> node server).
var jsFrames = []string{
	`{"dam":"hi","#":"g0yP076AP"}`,
	`{"#":"ETcmlsTM2","dam":"?","pid":"FSHvy61XC"}`,
	`{"dam":"?","pid":"4nwJ7VOyn","@":"ETcmlsTM2","#":"aPjXaTy9D"}`,
	`{"get":{".":"boss","#":"mark"},"#":"MyLLmbpXy"}`,
	`{"#":"47RmcdeDG","@":"MyLLmbpXy"}`,
	`{"put":{"mark":{"_":{"#":"mark",">":{"boss":1791147867592,"name":1791147867592}},"boss":{"#":"mark/boss"},"name":"Mark"},"mark/boss":{"_":{">":{"name":1791147867592},"#":"mark/boss"},"name":"Fluffy"}},"ok":{"@":9,"/":1},"#":"J0XPPtCHI"}`,
	`{"#":"jqmWH5Mnw","ok":{"/":1,"@":2},"put":{"mark":{"_":{"#":"mark",">":{"boss":1791147867592}},"boss":{"#":"mark/boss"}}}}`,
	`{"#":"7aE96WINI","@":"J0XPPtCHI","ok":true}`,
	`[{"put":{"list":{"_":{"#":"list",">":{"muub701jpN0KyGX7nZ2h":1791147867895.002}},"muub701jpN0KyGX7nZ2h":{"#":"muub701jpN0KyGX7nZ2h"}}},"ok":{"@":9,"/":1},"#":"ioDF9UppV"}]`,
	`{"#":"OBxr0UdxZ","##":-852707395,"@":"jixjaybNW","FOO":1,"put":{"mark":{"_":{"#":"mark",">":{"boss":1791147867592,"name":1791147867592}},"boss":{"#":"mark/boss"},"name":"Mark"}}}`,
	`{"#":"WbD4U5ud9","><":"MqFlORK97,VJcWpJ1CV","get":{"#":"mark"}}`,
	`[]`,
}

func TestReadJSFrames(t *testing.T) {
	for _, f := range jsFrames {
		msgs, err := readFrame([]byte(f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if f != "[]" && len(msgs) != 1 {
			t.Fatalf("%s: got %d messages", f, len(msgs))
		}
	}

	msgs, _ := readFrame([]byte(jsFrames[5]))
	m := msgs[0]
	if m.ID != "J0XPPtCHI" || len(m.Put) != 2 || string(m.OK) != `{"@":9,"/":1}` {
		t.Fatalf("put frame decoded wrong: %+v", m)
	}
	if l := m.Put["mark"].Fields["boss"]; l != (Link{Soul: "mark/boss"}) {
		t.Fatalf("boss = %#v", l)
	}

	msgs, _ = readFrame([]byte(jsFrames[3]))
	if q := msgs[0].Get; q.Soul != "mark" || q.Key != "boss" {
		t.Fatalf("get = %+v", q)
	}
	msgs, _ = readFrame([]byte(jsFrames[10]))
	if !msgs[0].sentTo("VJcWpJ1CV") || msgs[0].sentTo("other") {
		t.Fatal("><  hint not parsed")
	}
}

func TestBadMessageDoesNotDropBatch(t *testing.T) {
	frame := `[{"#":"a","put":{"x":{"_":{"#":"x",">":{}},"bad":1}}},{"#":"b","get":{"#":"y"}}]`
	msgs, err := readFrame([]byte(frame))
	if err == nil {
		t.Fatal("want error for the bad message")
	}
	if len(msgs) != 1 || msgs[0].ID != "b" {
		t.Fatalf("good message lost: %+v", msgs)
	}
}

func TestGetLex(t *testing.T) {
	var q getQuery
	if err := json.Unmarshal([]byte(`{"#":"chat",".":{"*":"2024-"},"%":50000}`), &q); err != nil {
		t.Fatal(err)
	}
	if q.Soul != "chat" || q.KeyLex == nil {
		t.Fatalf("%+v", q)
	}
	for k, want := range map[string]bool{"2024-01": true, "2023-12": false} {
		if q.match(k) != want {
			t.Errorf("match(%q) = %v", k, !want)
		}
	}
	// Relaying keeps the original JSON, including keys we don't model.
	b, _ := json.Marshal(&q)
	if !strings.Contains(string(b), `"%":50000`) {
		t.Fatalf("relayed get lost data: %s", b)
	}
}

func TestLexMatch(t *testing.T) {
	s := func(v string) *string { return &v }
	cases := []struct {
		l    lex
		t    string
		want bool
	}{
		{lex{Eq: s("a")}, "a", true},
		{lex{Eq: s("a")}, "ab", false},
		{lex{Prefix: s("ab")}, "abc", true},
		{lex{Prefix: s("ab")}, "b", false},
		{lex{Gt: s("b"), Lt: s("d")}, "c", true},
		{lex{Gt: s("b"), Lt: s("d")}, "e", false},
		{lex{Gt: s("b")}, "c", true},
		{lex{Lt: s("b")}, "a", true},
	}
	for _, c := range cases {
		if got := c.l.match(c.t); got != c.want {
			t.Errorf("%+v match %q = %v", c.l, c.t, got)
		}
	}
}

func TestMarshalWireKeepsHTML(t *testing.T) {
	n := NewNode("a<b")
	n.Set("x>y", String("<&>"), 1)
	b, err := marshalWire(&message{ID: "1", Put: graph{"a<b": n}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"#":"1","put":{"a<b":{"_":{"#":"a<b",">":{"x>y":1}},"x>y":"<&>"}}}`
	if string(b) != want {
		t.Fatalf("\n got %s\nwant %s", b, want)
	}
}
