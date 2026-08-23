package gundb

import (
	"bytes"
	"encoding/json"
)

// marshalWire encodes v without HTML-escaping and without the trailing
// newline json.Encoder adds, so the bytes can go straight onto the wire.
func marshalWire(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
