package gundb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
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
func (n *Node) MarshalJSON() ([]byte, error) {
	keys := slices.Sorted(maps.Keys(n.Fields))
	var buf bytes.Buffer
	buf.WriteString(`{"_":{"#":`)
	buf.WriteString(jsQuote(n.Soul))
	buf.WriteString(`,">":{`)
	for i, k := range keys {
		s, ok := n.States[k]
		if !ok {
			return nil, fmt.Errorf("gundb: node %q has no state for %q", n.Soul, k)
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(jsQuote(k))
		buf.WriteByte(':')
		buf.WriteString(jsonNumber(s))
	}
	buf.WriteString("}}")
	for _, k := range keys {
		// Call the variant's MarshalJSON directly: json.Marshal would
		// HTML-escape strings, which JS does not do.
		v, err := n.Fields[k].(json.Marshaler).MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("gundb: node %q field %q: %w", n.Soul, k, err)
		}
		buf.WriteByte(',')
		buf.WriteString(jsQuote(k))
		buf.WriteByte(':')
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// UnmarshalJSON parses the GUN wire form. Like the reference it rejects
// nodes without a soul, fields without a state, and invalid values.
func (n *Node) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("gundb: node: %w", err)
	}
	var meta struct {
		Soul   string             `json:"#"`
		States map[string]float64 `json:">"`
	}
	if m, ok := raw["_"]; !ok || json.Unmarshal(m, &meta) != nil || meta.Soul == "" {
		return fmt.Errorf("gundb: node has no valid _ metadata")
	}
	delete(raw, "_")
	*n = *NewNode(meta.Soul)
	for k, r := range raw {
		state, ok := meta.States[k]
		if !ok {
			return fmt.Errorf("gundb: node %q field %q has no state", meta.Soul, k)
		}
		v, err := decodeValue(r)
		if err != nil {
			return fmt.Errorf("gundb: node %q field %q: %w", meta.Soul, k, err)
		}
		n.Set(k, v, state)
	}
	return nil
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
