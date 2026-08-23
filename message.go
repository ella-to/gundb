package gundb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// message is one GUN wire message. Captured from a JS peer, typical ones are:
//
//	{"#":"Dc4LlyT4b","dam":"?","pid":"uICWDpxzd"}                      handshake
//	{"#":"MyLLmbpXy","get":{"#":"mark",".":"boss"}}                    read
//	{"#":"J0XPPtCHI","put":{"mark":{...}},"ok":{"@":9,"/":1}}          write
//	{"#":"7aE96WINI","@":"J0XPPtCHI","ok":true}                        ack
//	{"#":"OBxr0UdxZ","@":"jixjaybNW","##":-852707395,"put":{...}}      read reply
//
// Fields the runtime does not interpret (ok, ##, %, err) are kept as raw
// JSON so they survive relaying unchanged. Unknown keys are ignored.
type message struct {
	ID   string          `json:"#,omitempty"`
	Ack  string          `json:"@,omitempty"`
	Put  graph           `json:"put,omitempty"`
	Get  *getQuery       `json:"get,omitempty"`
	OK   json.RawMessage `json:"ok,omitempty"`
	Err  json.RawMessage `json:"err,omitempty"`
	Dam  string          `json:"dam,omitempty"`
	PID  string          `json:"pid,omitempty"`
	Hash json.RawMessage `json:"##,omitempty"`
	Sent string          `json:"><,omitempty"` // peers the sender already relayed to
	More json.RawMessage `json:"%,omitempty"`  // set on every chunk of a reply except the last
}

func (m *message) empty() bool {
	return m.ID == "" && m.Ack == "" && m.Dam == "" && m.Put == nil && m.Get == nil
}

// errText returns the "err" field as text, or "" if there is none.
func (m *message) errText() string {
	if len(m.Err) == 0 || string(m.Err) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(m.Err, &s) == nil {
		return s
	}
	return string(m.Err)
}

// sentTo reports whether the "><" hint says pid already has this message.
func (m *message) sentTo(pid string) bool {
	if m.Sent == "" || pid == "" {
		return false
	}
	for s := range strings.SplitSeq(m.Sent, ",") {
		if s == pid {
			return true
		}
	}
	return false
}

// getQuery is the body of a "get": {"#": soul} for a whole node or
// {"#": soul, ".": key} for one field. Both "#" and "." may also be LEX
// objects such as {"*": "prefix"}; the original JSON is kept so it can be
// relayed unchanged.
type getQuery struct {
	Soul   string
	Key    string
	KeyLex *lex
	raw    json.RawMessage
}

func (q *getQuery) UnmarshalJSON(data []byte) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("gundb: get: %w", err)
	}
	*q = getQuery{raw: bytes.Clone(data)}
	if r, ok := obj["#"]; ok && json.Unmarshal(r, &q.Soul) != nil {
		var l lex
		if json.Unmarshal(r, &l) == nil && l.Eq != nil {
			q.Soul = *l.Eq
		}
	}
	if r, ok := obj["."]; ok && json.Unmarshal(r, &q.Key) != nil {
		q.KeyLex = new(lex)
		if err := json.Unmarshal(r, q.KeyLex); err != nil {
			return fmt.Errorf("gundb: get key: %w", err)
		}
	}
	return nil
}

func (q *getQuery) MarshalJSON() ([]byte, error) {
	if q.raw != nil {
		return q.raw, nil
	}
	s := `{"#":` + jsQuote(q.Soul)
	if q.Key != "" {
		s += `,".":` + jsQuote(q.Key)
	}
	return []byte(s + "}"), nil
}

// match reports whether field name k is selected by the query.
func (q *getQuery) match(k string) bool {
	switch {
	case q.KeyLex != nil:
		return q.KeyLex.match(k)
	case q.Key != "":
		return q.Key == k
	}
	return true
}

// lex is a GUN LEX range: {"=": exact} | {"*": prefix} | {">": from, "<": to}.
type lex struct {
	Eq     *string `json:"="`
	Prefix *string `json:"*"`
	Gt     *string `json:">"`
	Lt     *string `json:"<"`
}

// match ports String.match from gun/src/shim.js, quirks included.
func (o *lex) match(t string) bool {
	truthy := func(s *string) bool { return s != nil && *s != "" }
	for _, s := range []*string{o.Eq, o.Prefix, o.Gt, o.Lt} {
		if truthy(s) {
			if t == *s {
				return true
			}
			break
		}
	}
	if o.Eq != nil {
		return false
	}
	if p := cmpOr(o.Prefix, o.Gt, truthy); p != nil && strings.HasPrefix(t, *p) {
		return true
	}
	if o.Prefix != nil {
		return false
	}
	if o.Gt != nil && o.Lt != nil {
		return jsCompare(t, *o.Gt) >= 0 && jsCompare(t, *o.Lt) <= 0
	}
	if o.Gt != nil && jsCompare(t, *o.Gt) >= 0 {
		return true
	}
	return o.Lt != nil && jsCompare(t, *o.Lt) <= 0
}

func cmpOr(a, b *string, ok func(*string) bool) *string {
	if ok(a) {
		return a
	}
	if ok(b) {
		return b
	}
	return nil
}

// heartbeat is the empty batch GUN servers send every 20s to keep proxies
// from closing idle WebSockets.
var heartbeat = []byte("[]")

// readFrame decodes one wire frame: a single message object or an array of
// them. Messages that fail to decode are skipped and reported in the error,
// so one bad message never drops the rest of a batch.
func readFrame(data []byte) ([]message, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	var raws []json.RawMessage
	switch data[0] {
	case '[':
		if err := json.Unmarshal(data, &raws); err != nil {
			return nil, fmt.Errorf("gundb: bad batch: %w", err)
		}
	case '{':
		raws = []json.RawMessage{data}
	default:
		return nil, fmt.Errorf("gundb: bad frame starting with %q", data[0])
	}
	msgs := make([]message, 0, len(raws))
	var errs []error
	for _, r := range raws {
		var m message
		if err := json.Unmarshal(r, &m); err != nil {
			errs = append(errs, err)
			continue
		}
		msgs = append(msgs, m)
	}
	return msgs, errors.Join(errs...)
}
