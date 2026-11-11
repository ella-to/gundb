package gundb

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

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

	// raw is the message as it was received, while it is unchanged: it
	// is passed on as it is instead of being encoded again.
	raw []byte
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
// them, in one pass. Messages that fail to decode are skipped and reported
// in the error, so one bad message never drops the rest of a batch; a frame
// that is not valid JSON is dropped whole.
//
// Each message keeps the bytes it was received as (message.raw), slices of
// data, to be passed on as they are.
func readFrame(data []byte) ([]message, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	dec := decoders.Get().(*jsontext.Decoder)
	defer decoders.Put(dec)
	// A bytes.Buffer is parsed in place, without copying it.
	dec.Reset(bytes.NewBuffer(data), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	switch data[0] {
	case '[':
		if _, err := dec.ReadToken(); err != nil {
			return nil, fmt.Errorf("gundb: bad batch: %w", err)
		}
		var msgs []message
		var errs []error
		for dec.PeekKind() != ']' {
			m, err := readMessage(dec, data)
			if err != nil {
				// Skip the rest of the bad message; JSON that is not well
				// formed drops the whole frame.
				for dec.StackDepth() > 1 {
					if _, err := dec.ReadToken(); err != nil {
						return nil, fmt.Errorf("gundb: bad batch: %w", err)
					}
				}
				if isSyntax(err) {
					return nil, fmt.Errorf("gundb: bad batch: %w", err)
				}
				errs = append(errs, err)
				continue
			}
			msgs = append(msgs, m)
		}
		if _, err := dec.ReadToken(); err != nil {
			return nil, fmt.Errorf("gundb: bad batch: %w", err)
		}
		if _, err := dec.ReadToken(); err != io.EOF {
			return nil, errors.New("gundb: bad batch: trailing data")
		}
		return msgs, errors.Join(errs...)
	case '{':
		m, err := readMessage(dec, data)
		if err == nil {
			if _, end := dec.ReadToken(); end != io.EOF {
				err = errors.New("gundb: trailing data")
			}
		}
		if err != nil {
			return nil, err
		}
		return []message{m}, nil
	default:
		return nil, fmt.Errorf("gundb: bad frame starting with %q", data[0])
	}
}

// decoders are reused across frames, as encoding/json reuses its own.
var decoders = sync.Pool{New: func() any { return jsontext.NewDecoder(bytes.NewBuffer(nil)) }}

// isSyntax reports whether err means the JSON is not well formed.
func isSyntax(err error) bool {
	var se *jsontext.SyntacticError
	return errors.As(err, &se) || errors.Is(err, io.ErrUnexpectedEOF)
}

// readMessage reads one message object from dec, which reads data. It
// accepts what json.Unmarshal into message accepted, with field names
// matched exactly.
func readMessage(dec *jsontext.Decoder, data []byte) (message, error) {
	var m message
	start := dec.InputOffset()
	switch dec.PeekKind() {
	case 'n': // null: an empty message, ignored
		_, err := dec.ReadToken()
		return m, err
	case '{':
	default:
		if err := dec.SkipValue(); err != nil {
			return m, err
		}
		return m, errors.New("gundb: message is not an object")
	}
	if _, err := dec.ReadToken(); err != nil {
		return m, err
	}
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return m, err
		}
		switch key := name.String(); key {
		case "#", "@", "dam", "pid", "><":
			s, err := readString(dec)
			if err != nil {
				return m, fmt.Errorf("gundb: message %q: %w", key, err)
			}
			switch key {
			case "#":
				m.ID = s
			case "@":
				m.Ack = s
			case "dam":
				m.Dam = s
			case "pid":
				m.PID = s
			default:
				m.Sent = s
			}
		case "put":
			if m.Put, err = readGraph(dec); err != nil {
				return m, err
			}
		case "get":
			if m.Get, err = readGet(dec, data); err != nil {
				return m, err
			}
		case "ok", "err", "##", "%":
			raw, err := readRaw(dec, data)
			if err != nil {
				return m, err
			}
			switch key {
			case "ok":
				m.OK = raw
			case "err":
				m.Err = raw
			case "##":
				m.Hash = raw
			default:
				m.More = raw
			}
		default:
			if err := dec.SkipValue(); err != nil {
				return m, err
			}
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return m, err
	}
	m.raw = data[start:dec.InputOffset()]
	m.raw = m.raw[bytes.IndexByte(m.raw, '{'):]
	return m, nil
}

// readString reads a string, or null as "".
func readString(dec *jsontext.Decoder) (string, error) {
	switch dec.PeekKind() {
	case '"', 'n':
		tok, err := dec.ReadToken()
		if err != nil || tok.Kind() == 'n' {
			return "", err
		}
		return tok.String(), nil
	}
	if err := dec.SkipValue(); err != nil {
		return "", err
	}
	return "", errors.New("not a string")
}

// readRaw reads any value and returns it as data holds it.
func readRaw(dec *jsontext.Decoder, data []byte) (json.RawMessage, error) {
	start := dec.InputOffset()
	if err := dec.SkipValue(); err != nil {
		return nil, err
	}
	raw := bytes.TrimLeft(data[start:dec.InputOffset()], " \t\r\n:")
	return json.RawMessage(raw), nil
}

// readGraph reads a put's {soul: node, ...}, or null.
func readGraph(dec *jsontext.Decoder) (graph, error) {
	switch dec.PeekKind() {
	case 'n':
		_, err := dec.ReadToken()
		return nil, err
	case '{':
	default:
		if err := dec.SkipValue(); err != nil {
			return nil, err
		}
		return nil, errors.New("gundb: put is not an object")
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, err
	}
	g := graph{}
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return nil, err
		}
		soul := name.String()
		if dec.PeekKind() == 'n' {
			if _, err := dec.ReadToken(); err != nil {
				return nil, err
			}
			g[soul] = nil // refused by check, like the reference
			continue
		}
		n := new(Node)
		if err := n.UnmarshalJSONFrom(dec); err != nil {
			return nil, err
		}
		g[soul] = n
	}
	_, err := dec.ReadToken()
	return g, err
}

// readGet reads a get's {"#": soul, ".": key}, or null. Either may be a
// LEX object instead; the get's JSON is kept, to pass it on unchanged.
func readGet(dec *jsontext.Decoder, data []byte) (*getQuery, error) {
	start := dec.InputOffset()
	switch dec.PeekKind() {
	case 'n':
		_, err := dec.ReadToken()
		return nil, err
	case '{':
	default:
		if err := dec.SkipValue(); err != nil {
			return nil, err
		}
		return nil, errors.New("gundb: get is not an object")
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, err
	}
	q := &getQuery{}
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return nil, err
		}
		switch name.String() {
		case "#": // the last "#" counts
			q.Soul, q.SoulLex = "", nil
			switch dec.PeekKind() {
			case '"':
				tok, err := dec.ReadToken()
				if err != nil {
					return nil, err
				}
				q.Soul = tok.String()
			case '{':
				raw, err := dec.ReadValue()
				if err != nil {
					return nil, err
				}
				var l lex
				switch {
				case json.Unmarshal(raw, &l) != nil: // not a soul; answered as not found
				case l.Eq != nil:
					q.Soul = *l.Eq
				default:
					q.SoulLex = &l
				}
			default: // not a soul; answered as not found
				if err := dec.SkipValue(); err != nil {
					return nil, err
				}
			}
		case ".":
			q.Key, q.KeyLex = "", nil
			switch dec.PeekKind() {
			case '"', 'n':
				if q.Key, err = readString(dec); err != nil {
					return nil, err
				}
			default:
				raw, err := dec.ReadValue()
				if err != nil {
					return nil, err
				}
				q.KeyLex = new(lex)
				if err := json.Unmarshal(raw, q.KeyLex); err != nil {
					return nil, fmt.Errorf("gundb: get key: %w", err)
				}
			}
		default:
			if err := dec.SkipValue(); err != nil {
				return nil, err
			}
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, err
	}
	raw := bytes.TrimLeft(data[start:dec.InputOffset()], " \t\r\n:")
	q.raw = json.RawMessage(raw)
	return q, nil
}
