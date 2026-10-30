package gundb

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"maps"
	"testing"
)

// refUnmarshalNode is Node.UnmarshalJSON as it was before "_" and links
// were read from tokens: the reference the fast decoder must match.
func refUnmarshalNode(data []byte) (*Node, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(data), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '{' {
		return nil, fmt.Errorf("not an object")
	}
	var meta struct {
		Soul   string             `json:"#"`
		States map[string]float64 `json:">"`
	}
	hasMeta := false
	fields := map[string]Value{}
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return nil, err
		}
		k := name.String()
		if k == "_" {
			raw, err := dec.ReadValue()
			if err != nil {
				return nil, err
			}
			meta.Soul, meta.States = "", nil
			hasMeta = json.Unmarshal(raw, &meta) == nil
			continue
		}
		var v Value
		switch dec.PeekKind() {
		case 'n', 't', 'f', '"', '0':
			v, err = readValue(dec)
		default:
			var raw jsontext.Value
			if raw, err = dec.ReadValue(); err == nil {
				v, err = decodeValue(json.RawMessage(raw))
			}
		}
		if err != nil {
			return nil, err
		}
		fields[k] = v
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, err
	}
	if _, err := dec.ReadToken(); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	if !hasMeta || meta.Soul == "" {
		return nil, fmt.Errorf("no valid _")
	}
	n := &Node{Soul: meta.Soul, Fields: fields, States: map[string]float64{}}
	for k := range fields {
		state, ok := meta.States[k]
		if !ok {
			return nil, fmt.Errorf("no state")
		}
		n.States[k] = state
	}
	return n, nil
}

func checkSameNode(t *testing.T, data []byte) {
	want, werr := refUnmarshalNode(data)
	var got Node
	gerr := got.UnmarshalJSON(data)
	if (werr == nil) != (gerr == nil) {
		t.Fatalf("%s:\n reference err=%v\n      fast err=%v", data, werr, gerr)
	}
	if werr != nil {
		return
	}
	if got.Soul != want.Soul || !maps.Equal(got.Fields, want.Fields) || !maps.Equal(got.States, want.States) {
		t.Fatalf("%s:\n reference %+v\n      fast %+v", data, want, &got)
	}
	// Inside a message, json.Unmarshal drives the decoder.
	var viaJSON Node
	if err := json.Unmarshal(data, &viaJSON); err != nil {
		t.Fatalf("%s: json.Unmarshal: %v", data, err)
	}
	if viaJSON.Soul != want.Soul || !maps.Equal(viaJSON.Fields, want.Fields) || !maps.Equal(viaJSON.States, want.States) {
		t.Fatalf("%s:\n reference %+v\n  via json %+v", data, want, &viaJSON)
	}
}

var nodeSeeds = []string{
	putNode,
	`{"_":{"#":"a",">":{"x":1}},"x":{"#":"b"}}`,
	`{"_":{"#":"a",">":{"x":1}},"x":{"#":"b","#":"c"}}`,
	`{"_":{"#":"a",">":{"x":1}},"x":{"#":"b","#":null}}`,
	`{"_":{"#":"a",">":{"x":1}},"x":{"#":null,"#":"b"}}`,
	`{"_":{"#":"a",">":{"x":1}},"x":{"#":1}}`,
	`{"_":{"#":"a",">":{"x":1}},"x":{"#":""}}`,
	`{"_":{"#":"a",">":{"x":1}},"x":{"#":"b","y":1}}`,
	`{"_":{"#":"a",">":{"x":1}},"x":{}}`,
	`{"_":{"#":"a",">":{"x":1}},"x":[1]}`,
	`{"_":{"#":"a",">":{"x":1}},"x":{"#":{"#":"b"}}}`,
	`{"_":{"#":"a",">":{"x":null}},"x":1}`,
	`{"_":{"#":"a",">":{"x":"1"}},"x":1}`,
	`{"_":{"#":"a",">":{"x":1e400}},"x":1}`,
	`{"_":{"#":"a",">":{"x":1},">":{"y":2}},"x":1,"y":2}`,
	`{"_":{"#":"a",">":{"x":1},">":null},"x":1}`,
	`{"_":{"#":"a",">":null,">":{"x":1}},"x":1}`,
	`{"_":{"#":"a","#":null,">":{"x":1}},"x":1}`,
	`{"_":{"#":null,">":{"x":1}},"x":1}`,
	`{"_":{"#":1,">":{"x":1}},"x":1}`,
	`{"_":{"#":"a",">":[1]},"x":1}`,
	`{"_":{"#":"a",">":{"x":1},"other":[1,{"z":2}]},"x":1}`,
	`{"_":"nope","x":1}`,
	`{"_":null,"x":1}`,
	`{"_":{"#":"a",">":{"x":1}},"_":{"#":"b",">":{"x":2}},"x":1}`,
	`{"_":{"#":"a",">":{"x":1}},"x":1,"_":{"#":"b"}}`,
	`{"_":{"#":"a",">":{"x":-0}},"x":1}`,
	`{"_":{"#":"a\u0000b",">":{"x\u00e9":1}},"x\u00e9":"\ud83c\udf89"}`,
}

// putNode is the node inside putFrame.
const putNode = `{"_":{"#":"users/alice",">":{"name":1700000000000,"age":1700000000000,"bio":1700000000000,"boss":1700000000000}},"name":"Alice","age":30,"bio":"Writes Go & JS <code> all day","boss":{"#":"users/bob"}}`

func TestNodeTokenDecodeMatchesReference(t *testing.T) {
	for _, s := range nodeSeeds {
		checkSameNode(t, []byte(s))
	}
}

func FuzzNodeTokenDecode(f *testing.F) {
	for _, s := range nodeSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(checkSameNode)
}
