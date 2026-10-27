package gundb

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"testing"
)

// marshalWire is the reference encoding: encoding/json without HTML
// escaping. appendWire must produce the same bytes.
func marshalWire(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func TestAppendWireMatchesEncodingJSON(t *testing.T) {
	n := NewNode("users/a<b>")
	n.Set("name", String("Ali \"A\" & <co>\n\x01"), 1700000000000.5)
	n.Set("age", Number(30), 1)
	n.Set("tiny", Number(1e-7), 2)
	n.Set("big", Number(1.5e22), 3)
	n.Set("ok", Bool(true), 4)
	n.Set("no", Bool(false), 4)
	n.Set("gone", Null{}, 5)
	n.Set("boss", Link{Soul: "users/😀"}, 6)
	n.Set("", String("é"), 7)
	frames, err := readFrame([]byte(`[` +
		putFrame + `,` +
		`{"#":"a","get":{"#":"x",".":{"*":"p"}}},` +
		`{"#":"b","get":{"#": {"*": "chat/"}, "%": 100000}},` +
		`{"#":"c","@":"d","ok": {"@": 9},"err":"boom","##":-852707395,"%":1,"><":"p1,p2"},` +
		`{"#":"e","dam":"?","pid":"me"}]`))
	if err != nil {
		t.Fatal(err)
	}
	msgs := []*message{
		{ID: "1", Put: graph{n.Soul: n, "other": NewNode("other")}},
		{ID: "2", Get: &getQuery{Soul: "a\"b", Key: "k"}},
		{ID: "3", Get: &getQuery{SoulLex: &lex{Prefix: new("chat/")}}},
		{ID: "4", Ack: "x", OK: okAck},
		{ID: "5", Ack: "x", Err: errJSON(ErrForbidden)},
		{Dam: "?", PID: "p"},
		{},
	}
	for i := range frames {
		msgs = append(msgs, &frames[i])
	}
	for _, m := range msgs {
		want, err := marshalWire(m)
		if err != nil {
			t.Fatal(err)
		}
		got, err := m.appendWire(nil)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("\n got %s\nwant %s", got, want)
		}
	}
}

func TestAppendWireRejectsBadNodes(t *testing.T) {
	nan := NewNode("a")
	nan.Set("x", Number(math.NaN()), 1)
	noState := NewNode("b")
	noState.Fields["x"] = String("y")
	for _, n := range []*Node{nan, noState} {
		if _, err := (&message{Put: graph{n.Soul: n}}).appendWire(nil); err == nil {
			t.Errorf("node %s should not encode", n.Soul)
		}
	}
}

// oldUnmarshalNode is the reference decoding the streaming one replaced.
func oldUnmarshalNode(data []byte) (*Node, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	var meta struct {
		Soul   string             `json:"#"`
		States map[string]float64 `json:">"`
	}
	if m, ok := raw["_"]; !ok || json.Unmarshal(m, &meta) != nil || meta.Soul == "" {
		return nil, errors.New("no meta")
	}
	delete(raw, "_")
	n := NewNode(meta.Soul)
	for k, r := range raw {
		state, ok := meta.States[k]
		if !ok {
			return nil, errors.New("no state")
		}
		v, err := decodeValue(r)
		if err != nil {
			return nil, err
		}
		n.Set(k, v, state)
	}
	return n, nil
}

func TestNodeDecodeMatchesReference(t *testing.T) {
	for _, in := range []string{
		`{"_":{"#":"a",">":{"x":1,"y":2,"z":3,"w":4,"l":5}},"x":"hi \"there\"\n\u00e9","y":-1.5e3,"z":true,"w":null,"l":{"#":"b"}}`,
		`{"x":1,"_":{"#":"a",">":{"x":7}}}`,                           // metadata last
		`{"_":{"#":"a",">":{"x":1}},"_":{"#":"b",">":{"x":2}},"x":3}`, // repeated "_"
		`{"_":{"#":"a",">":{"x":1}},"x":1,"x":2}`,                     // repeated field
		`{ "_" : { "#" : "a" , ">" : { "x" : 1 } } , "x" : "s" }`,
		`{"_":{"#":"a",">":{"x":1}},"x":"bad` + "\xff" + `utf8"}`,
		`{"_":{"#":"a",">":{"x":1},"extra":[1]},"x":1}`,
		`{"_":{"#":"a",">":{}}}`,
		`{"_":{"#":"a",">":{"x":1}},"x":1e400}`,
		`{"_":{"#":"a",">":{"x":1}},"x":[1]}`,
		`{"_":{"#":"a",">":{"x":1}},"x":{"a":1}}`,
		`{"_":{"#":"a",">":{"x":1}},"x":{"#":""}}`,
		`{"_":{"#":"a",">":{}},"x":1}`,
		`{"_":{"#":"",">":{}}}`,
		`{"_":{"#":1}}`,
		`{"_":null}`,
		`{"x":1}`,
		`{}`, `[]`, `"a"`, `null`, `{"_":{"#":"a"}} x`,
	} {
		want, werr := oldUnmarshalNode([]byte(in))
		var got Node
		gerr := json.Unmarshal([]byte(in), &got)
		var direct Node
		if derr := direct.UnmarshalJSON([]byte(in)); (derr != nil) != (gerr != nil) {
			t.Errorf("%s: UnmarshalJSON err = %v, json.Unmarshal err = %v", in, derr, gerr)
		}
		if (werr != nil) != (gerr != nil) {
			t.Errorf("%s: err = %v, want %v", in, gerr, werr)
			continue
		}
		if werr == nil && (got.Soul != want.Soul || !maps.Equal(got.Fields, want.Fields) || !maps.Equal(got.States, want.States)) {
			t.Errorf("%s:\n got %+v\nwant %+v", in, got, *want)
		}
	}
}
