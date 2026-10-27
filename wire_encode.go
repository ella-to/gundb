package gundb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"

	"ella.to/gundb/internal/js"
)

// appendWire appends m as one wire message: the JSON encoding/json would
// produce for it, but written straight into b with JS string escaping (no
// HTML escapes) and without re-parsing what each field's MarshalJSON wrote.
// Every message we send or relay goes through here.
func (m *message) appendWire(b []byte) ([]byte, error) {
	b = append(b, '{')
	str := func(k, v string) {
		if v != "" {
			b = appendKey(b, k)
			b = js.AppendQuote(b, v)
		}
	}
	raw := func(k string, v json.RawMessage) {
		if len(v) > 0 {
			b = appendKey(b, k)
			b = appendRaw(b, v)
		}
	}
	str("#", m.ID)
	str("@", m.Ack)
	if len(m.Put) > 0 {
		b = appendKey(b, "put")
		var err error
		if b, err = m.Put.appendJSON(b); err != nil {
			return nil, err
		}
	}
	if m.Get != nil {
		b = appendKey(b, "get")
		b = m.Get.appendJSON(b)
	}
	raw("ok", m.OK)
	raw("err", m.Err)
	str("dam", m.Dam)
	str("pid", m.PID)
	raw("##", m.Hash)
	str("><", m.Sent)
	raw("%", m.More)
	return append(b, '}'), nil
}

// appendKey appends a comma unless k is the first key of the object, then
// the quoted key and a colon.
func appendKey(b []byte, k string) []byte {
	if b[len(b)-1] != '{' {
		b = append(b, ',')
	}
	return append(js.AppendQuote(b, k), ':')
}

// appendRaw appends JSON we hold verbatim, compacted the way encoding/json
// would. It came from a parsed message or from us, so it is valid.
func appendRaw(b []byte, raw json.RawMessage) []byte {
	if !bytes.ContainsAny(raw, " \t\r\n") {
		return append(b, raw...)
	}
	buf := bytes.NewBuffer(b)
	if err := json.Compact(buf, raw); err != nil {
		return append(b, raw...)
	}
	return buf.Bytes()
}

// appendJSON appends g as {soul: node, ...} with souls sorted.
func (g graph) appendJSON(b []byte) ([]byte, error) {
	b = append(b, '{')
	for i, soul := range slices.Sorted(maps.Keys(g)) {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(js.AppendQuote(b, soul), ':')
		if g[soul] == nil {
			b = append(b, "null"...)
			continue
		}
		var err error
		if b, err = g[soul].appendJSON(b); err != nil {
			return nil, err
		}
	}
	return append(b, '}'), nil
}

// appendJSON appends the GUN wire form of n with sorted keys.
func (n *Node) appendJSON(b []byte) ([]byte, error) {
	keys := slices.Sorted(maps.Keys(n.Fields))
	b = append(b, `{"_":{"#":`...)
	b = js.AppendQuote(b, n.Soul)
	b = append(b, `,">":{`...)
	for i, k := range keys {
		s, ok := n.States[k]
		if !ok {
			return nil, fmt.Errorf("gundb: node %q has no state for %q", n.Soul, k)
		}
		if i > 0 {
			b = append(b, ',')
		}
		b = append(js.AppendQuote(b, k), ':')
		b = append(b, js.Number(s)...)
	}
	b = append(b, "}}"...)
	for _, k := range keys {
		b = append(js.AppendQuote(append(b, ','), k), ':')
		var err error
		if b, err = appendValue(b, n.Fields[k]); err != nil {
			return nil, fmt.Errorf("gundb: node %q field %q: %w", n.Soul, k, err)
		}
	}
	return append(b, '}'), nil
}

// appendValue appends the JSON of one field value; see Value.
func appendValue(b []byte, v Value) ([]byte, error) {
	switch x := v.(type) {
	case Null:
		return append(b, "null"...), nil
	case Bool:
		if x {
			return append(b, "true"...), nil
		}
		return append(b, "false"...), nil
	case Number:
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("gundb: %v is not a valid GUN number", f)
		}
		return append(b, js.Number(f)...), nil
	case String:
		return js.AppendQuote(b, string(x)), nil
	case Link:
		return append(js.AppendQuote(append(b, `{"#":`...), x.Soul), '}'), nil
	}
	return nil, errors.New("gundb: missing value")
}
