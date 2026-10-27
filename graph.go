package gundb

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"maps"
)

// Node is one vertex of the graph: a soul (its globally unique ID) plus
// fields, each with a value and the HAM state it was written at.
//
// On the wire a node looks like:
//
//	{"_":{"#":"mark",">":{"name":1700000000000}},"name":"Mark"}
type Node struct {
	Soul   string
	Fields map[string]Value
	States map[string]float64
}

// NewNode returns an empty node.
func NewNode(soul string) *Node {
	return &Node{Soul: soul, Fields: map[string]Value{}, States: map[string]float64{}}
}

// Set writes a field with an explicit state. It does not run HAM.
func (n *Node) Set(field string, v Value, state float64) {
	n.Fields[field] = v
	n.States[field] = state
}

// Clone returns a copy that shares no maps with n.
func (n *Node) Clone() *Node {
	return &Node{Soul: n.Soul, Fields: maps.Clone(n.Fields), States: maps.Clone(n.States)}
}

// MarshalJSON writes the GUN wire form with sorted keys.
func (n *Node) MarshalJSON() ([]byte, error) { return n.appendJSON(nil) }

// UnmarshalJSON parses the GUN wire form. Like the reference it rejects
// nodes without a soul, fields without a state, and invalid values.
func (n *Node) UnmarshalJSON(data []byte) error {
	dec := jsontext.NewDecoder(bytes.NewReader(data), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	if err := n.UnmarshalJSONFrom(dec); err != nil {
		return err
	}
	if _, err := dec.ReadToken(); err != io.EOF {
		return errors.New("gundb: node: trailing data")
	}
	return nil
}

// UnmarshalJSONFrom is UnmarshalJSON reading straight from the decoder of
// the enclosing message, so a node is parsed once, in a single pass.
func (n *Node) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '{' {
		return fmt.Errorf("gundb: node: not an object")
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
			return fmt.Errorf("gundb: node: %w", err)
		}
		k := name.String()
		if k == "_" {
			raw, err := dec.ReadValue()
			if err != nil {
				return fmt.Errorf("gundb: node: %w", err)
			}
			meta.Soul, meta.States = "", nil // a repeated "_" replaces the first
			hasMeta = json.Unmarshal(raw, &meta) == nil
			continue
		}
		v, err := readValue(dec)
		if err != nil {
			return fmt.Errorf("gundb: node field %q: %w", k, err)
		}
		fields[k] = v
	}
	if _, err := dec.ReadToken(); err != nil {
		return fmt.Errorf("gundb: node: %w", err)
	}
	if !hasMeta || meta.Soul == "" {
		return fmt.Errorf("gundb: node has no valid _ metadata")
	}
	*n = Node{Soul: meta.Soul, Fields: fields, States: make(map[string]float64, len(fields))}
	for k := range fields {
		state, ok := meta.States[k]
		if !ok {
			return fmt.Errorf("gundb: node %q field %q has no state", meta.Soul, k)
		}
		n.States[k] = state
	}
	return nil
}

// readValue reads one field value. Scalars are taken from their token;
// objects (links) and anything else go through decodeValue.
func readValue(dec *jsontext.Decoder) (Value, error) {
	switch dec.PeekKind() {
	case 'n', 't', 'f', '"', '0':
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, err
		}
		switch tok.Kind() {
		case 'n':
			return Null{}, nil
		case 't', 'f':
			return Bool(tok.Bool()), nil
		case '"':
			return String(tok.String()), nil
		}
		f, err := tok.Float()
		if err != nil {
			return nil, fmt.Errorf("gundb: invalid number: %w", err)
		}
		return Number(f), nil
	}
	raw, err := dec.ReadValue()
	if err != nil {
		return nil, err
	}
	return decodeValue(json.RawMessage(raw))
}

// graph is soul -> node: the payload of a "put" message.
type graph map[string]*Node

// node returns g[soul], creating it if needed.
func (g graph) node(soul string) *Node {
	n, ok := g[soul]
	if !ok {
		n = NewNode(soul)
		g[soul] = n
	}
	return n
}
