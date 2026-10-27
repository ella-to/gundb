package gundb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"ella.to/gundb/internal/js"
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
// relayed unchanged. A LEX over souls selects many nodes; like radisk, "."
// is then ignored.
type getQuery struct {
	Soul    string
	SoulLex *lex
	Key     string
	KeyLex  *lex
	raw     json.RawMessage
}

func (q *getQuery) UnmarshalJSON(data []byte) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("gundb: get: %w", err)
	}
	*q = getQuery{raw: bytes.Clone(data)}
	if r, ok := obj["#"]; ok && json.Unmarshal(r, &q.Soul) != nil {
		var l lex
		switch {
		case json.Unmarshal(r, &l) != nil: // not a soul; answered as not found
		case l.Eq != nil:
			q.Soul = *l.Eq
		default:
			q.SoulLex = &l
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

func (q *getQuery) MarshalJSON() ([]byte, error) { return q.appendJSON(nil), nil }

func (q *getQuery) appendJSON(b []byte) []byte {
	if q.raw != nil {
		return appendRaw(b, q.raw)
	}
	if q.SoulLex != nil {
		return append(append(append(b, `{"#":`...), q.SoulLex.json()...), `,"%":`+strconv.Itoa(lexBudget)+`}`...)
	}
	b = js.AppendQuote(append(b, `{"#":`...), q.Soul)
	if q.Key != "" {
		b = js.AppendQuote(append(b, `,".":`...), q.Key)
	}
	return append(b, '}')
}

// lexBudget is the "%" we send with a LEX over souls: how many bytes of
// matches radisk may read for one reply. Without it radisk returns only the
// first match; 100000 is the most it allows (gun/lib/store.js).
const lexBudget = 100000

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

// matchSoul reports whether soul is selected by a LEX over souls. radisk
// reads one as a prefix plus an inclusive range (in JS string order), not
// with String.match's either/or rules.
func (o *lex) matchSoul(soul string) bool {
	if o.Eq != nil {
		return soul == *o.Eq
	}
	return (o.Prefix == nil || strings.HasPrefix(soul, *o.Prefix)) &&
		(o.Gt == nil || jsCompare(soul, *o.Gt) >= 0) &&
		(o.Lt == nil || jsCompare(soul, *o.Lt) <= 0)
}

// json encodes o with keys in a fixed order.
func (o *lex) json() string {
	var parts []string
	for _, f := range []struct {
		k string
		v *string
	}{{"=", o.Eq}, {"*", o.Prefix}, {">", o.Gt}, {"<", o.Lt}} {
		if f.v != nil {
			parts = append(parts, jsQuote(f.k)+":"+jsQuote(*f.v))
		}
	}
	return "{" + strings.Join(parts, ",") + "}"
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
