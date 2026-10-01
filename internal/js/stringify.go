package js

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
)

// Restringify returns JSON.stringify(JSON.parse(data)): the same value with
// object keys in their original order (a repeated key keeps its first
// position and its last value, as in JavaScript), numbers and strings
// formatted the JavaScript way, and no insignificant whitespace.
//
// SEA signs and hashes this form, so it has to match byte for byte.
func Restringify(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := parse(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, errors.New("js: trailing data after JSON value")
	}
	var b bytes.Buffer
	v.write(&b)
	return b.Bytes(), nil
}

// value is a parsed JSON value that remembers object key order.
type value struct {
	scalar string   // already formatted, for anything but objects and arrays
	keys   []string // object keys, in order
	obj    map[string]*value
	arr    []*value
	kind   byte // 's'calar, 'o'bject, 'a'rray
}

func parse(dec *json.Decoder) (*value, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			v := &value{kind: 'o', obj: map[string]*value{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k := kt.(string)
				child, err := parse(dec)
				if err != nil {
					return nil, err
				}
				if _, dup := v.obj[k]; !dup {
					v.keys = append(v.keys, k)
				}
				v.obj[k] = child
			}
			_, err := dec.Token()
			return v, err
		case '[':
			v := &value{kind: 'a'}
			for dec.More() {
				child, err := parse(dec)
				if err != nil {
					return nil, err
				}
				v.arr = append(v.arr, child)
			}
			_, err := dec.Token()
			return v, err
		}
		return nil, errors.New("js: unexpected delimiter")
	case string:
		return &value{kind: 's', scalar: Quote(t)}, nil
	case json.Number:
		// Out of range numbers are not an error in JavaScript: they parse to
		// ±Infinity, which stringifies as null, or to 0.
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, err
		}
		if math.IsInf(f, 0) {
			return &value{kind: 's', scalar: "null"}, nil
		}
		return &value{kind: 's', scalar: Number(f)}, nil
	case bool:
		return &value{kind: 's', scalar: strconv.FormatBool(t)}, nil
	case nil:
		return &value{kind: 's', scalar: "null"}, nil
	}
	return nil, errors.New("js: unexpected token")
}

func (v *value) write(b *bytes.Buffer) {
	switch v.kind {
	case 'o':
		b.WriteByte('{')
		for i, k := range v.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(Quote(k))
			b.WriteByte(':')
			v.obj[k].write(b)
		}
		b.WriteByte('}')
	case 'a':
		b.WriteByte('[')
		for i, c := range v.arr {
			if i > 0 {
				b.WriteByte(',')
			}
			c.write(b)
		}
		b.WriteByte(']')
	default:
		b.WriteString(v.scalar)
	}
}
