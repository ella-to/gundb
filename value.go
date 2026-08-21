package gundb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// Value is a leaf value stored in a node field. It is a closed set that
// mirrors what GUN accepts on the wire:
//
//	null            -> Null   (also the tombstone for deleted fields)
//	true / false    -> Bool
//	123.45          -> Number
//	"hello"         -> String
//	{"#": "soul"}   -> Link   (a reference to another node)
//
// Most code never touches Value directly: Ref.Put and Ref.Once convert
// to and from ordinary Go types. Value matters when you implement a Store.
type Value interface{ isValue() }

// Null is the JSON null value. GUN uses null as the tombstone for deleted fields.
type Null struct{}

// Bool is a JSON boolean.
type Bool bool

// Number is a JSON number. GUN, like JavaScript, has a single float64 number type.
type Number float64

// String is a JSON string.
type String string

// Link is a reference to another node, written on the wire as {"#": "soul"}.
type Link struct{ Soul string }

func (Null) isValue()   {}
func (Bool) isValue()   {}
func (Number) isValue() {}
func (String) isValue() {}
func (Link) isValue()   {}

func (Null) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

func (b Bool) MarshalJSON() ([]byte, error) {
	if b {
		return []byte("true"), nil
	}
	return []byte("false"), nil
}

func (n Number) MarshalJSON() ([]byte, error) {
	f := float64(n)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("gundb: %v is not a valid GUN number", f)
	}
	return []byte(jsonNumber(f)), nil
}

func (s String) MarshalJSON() ([]byte, error) { return []byte(jsQuote(string(s))), nil }

func (l Link) MarshalJSON() ([]byte, error) { return []byte(`{"#":` + jsQuote(l.Soul) + `}`), nil }

// lexical returns JSON.stringify(v), the string HAM compares to break ties
// between two writes that carry the same state. nil (an absent value)
// stringifies to "" just like undefined does in the reference.
func lexical(v Value) string {
	switch x := v.(type) {
	case Null:
		return "null"
	case Bool:
		return strconv.FormatBool(bool(x))
	case Number:
		return jsonNumber(float64(x))
	case String:
		return jsQuote(string(x))
	case Link:
		return `{"#":` + jsQuote(x.Soul) + `}`
	}
	return ""
}

// plain converts a Value into the Go value encoding/json produces for it, so
// node contents can be re-marshalled into user types. Links become {"#": soul}.
func plain(v Value) any {
	switch x := v.(type) {
	case Bool:
		return bool(x)
	case Number:
		return float64(x)
	case String:
		return string(x)
	case Link:
		return map[string]any{"#": x.Soul}
	}
	return nil
}

// decodeValue parses one JSON value into the matching Value variant. Objects
// are only valid when they are a link: exactly one key "#" holding a string.
func decodeValue(raw json.RawMessage) (Value, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errors.New("gundb: empty value")
	}
	switch raw[0] {
	case 'n':
		if string(raw) != "null" {
			return nil, fmt.Errorf("gundb: invalid value %s", raw)
		}
		return Null{}, nil
	case 't', 'f':
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("gundb: invalid value %s", raw)
		}
		return Bool(b), nil
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("gundb: invalid string: %w", err)
		}
		return String(s), nil
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("gundb: invalid object: %w", err)
		}
		var soul string
		if len(obj) != 1 || json.Unmarshal(obj["#"], &soul) != nil || soul == "" {
			return nil, fmt.Errorf("gundb: object value is not a link: %s", raw)
		}
		return Link{Soul: soul}, nil
	case '[':
		return nil, errors.New("gundb: arrays are not valid GUN values")
	default:
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("gundb: invalid number: %w", err)
		}
		return Number(f), nil
	}
}
