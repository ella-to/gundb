package gundb

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"maps"
	"strconv"
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
func (n *Node) MarshalJSON() ([]byte, error) { return n.appendJSON(nil, true) }

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
	var meta nodeMeta
	hasMeta := false
	fields := map[string]Value{}
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return fmt.Errorf("gundb: node: %w", err)
		}
		k := name.String()
		if k == "_" {
			meta = nodeMeta{} // a repeated "_" replaces the first
			ok, err := meta.read(dec)
			if err != nil {
				return fmt.Errorf("gundb: node: %w", err)
			}
			hasMeta = ok
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

// nodeMeta is a node's "_": {"#": soul, ">": {field: state}}.
type nodeMeta struct {
	Soul   string
	States map[string]float64
}

// read reads "_" from its tokens. It accepts what json.Unmarshal into
// {Soul string `json:"#"`; States map[string]float64 `json:">"`} accepts,
// with the same results: ok is false where that would fail on a type, and
// err is set only for JSON that is not well formed.
func (m *nodeMeta) read(dec *jsontext.Decoder) (ok bool, err error) {
	if dec.PeekKind() != '{' {
		_, err := dec.ReadValue()
		return false, err // not an object: no metadata
	}
	if _, err := dec.ReadToken(); err != nil {
		return false, err
	}
	ok = true
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return false, err
		}
		switch name.String() {
		case "#":
			switch dec.PeekKind() {
			case '"':
				tok, err := dec.ReadToken()
				if err != nil {
					return false, err
				}
				m.Soul = tok.String()
				continue
			case 'n': // null leaves it as it was
			default:
				ok = false
			}
		case ">":
			switch dec.PeekKind() {
			case '{':
				statesOK, err := m.readStates(dec)
				if err != nil {
					return false, err
				}
				ok = ok && statesOK
				continue
			case 'n':
				m.States = nil
			default:
				ok = false
			}
		}
		if _, err := dec.ReadValue(); err != nil {
			return false, err
		}
	}
	_, err = dec.ReadToken()
	return ok, err
}

// readStates reads {field: state} into m.States, merging like json does
// when ">" repeats. ok is false if a state is not a number.
func (m *nodeMeta) readStates(dec *jsontext.Decoder) (ok bool, err error) {
	if _, err := dec.ReadToken(); err != nil {
		return false, err
	}
	if m.States == nil {
		m.States = map[string]float64{}
	}
	ok = true
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return false, err
		}
		k := name.String()
		switch dec.PeekKind() {
		case '0':
			tok, err := dec.ReadToken()
			if err != nil {
				return false, err
			}
			if f, err := strconv.ParseFloat(tok.String(), 64); err == nil {
				m.States[k] = f
			} else {
				ok = false // out of range, as json reports it
			}
		case 'n':
			if _, err := dec.ReadToken(); err != nil {
				return false, err
			}
			m.States[k] = 0 // json stores the zero value for null
		default:
			ok = false
			if _, err := dec.ReadValue(); err != nil {
				return false, err
			}
		}
	}
	_, err = dec.ReadToken()
	return ok, err
}

// readLink reads an object value, which must be a link: {"#": "soul"},
// with the same rules as decodeValue.
func readLink(dec *jsontext.Decoder) (Value, error) {
	if _, err := dec.ReadToken(); err != nil {
		return nil, err
	}
	var soul string
	isSoul, other := false, false
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return nil, err
		}
		key := name.String() // a token is only valid until the next read
		if key == "#" && dec.PeekKind() == '"' {
			tok, err := dec.ReadToken()
			if err != nil {
				return nil, err
			}
			soul, isSoul = tok.String(), true // a repeated "#": the last one counts
			continue
		}
		if key == "#" {
			soul, isSoul = "", false // the last "#" is not a string
		} else {
			other = true
		}
		if _, err := dec.ReadValue(); err != nil {
			return nil, err
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, err
	}
	if other || !isSoul || soul == "" {
		return nil, errors.New("gundb: object value is not a link")
	}
	return Link{Soul: soul}, nil
}

// readValue reads one field value. Scalars are taken from their token,
// links from theirs; anything else goes through decodeValue.
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
	case '{':
		return readLink(dec)
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
