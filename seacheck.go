package gundb

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"ella.to/gundb/sea"
)

// ErrUnverified is returned when a write breaks SEA's rules: unsigned or
// badly signed data in a user's space (~pub), a forged alias (~@alias), or
// content that does not match its hash (souls with #).
var ErrUnverified = errors.New("gundb: SEA: unverified data")

// checked reports the fields of a graph whose SEA check already passed, or
// that we signed ourselves a moment ago, so they aren't verified again. A
// nil checked means every field is checked.
type checked func(soul, field string) bool

// allChecked is for a graph that already passed checkSEA.
func allChecked(string, string) bool { return true }

// checkSEA applies the checks every SEA peer runs on incoming data
// (gun/sea.js, check). Every DB runs them on every write, local or remote,
// like GUN.js peers do, so forged user data is never stored or relayed.
// Fields skip says were checked are left alone.
//
// The cheap checks run first. Signatures, which cost far more, are then
// verified in parallel when there are several.
func checkSEA(g graph, skip checked) error {
	var sigs []fieldRef
	for soul, n := range g {
		for field, v := range n.Fields {
			if skip != nil && skip(soul, field) {
				continue
			}
			if signedField(soul, field) {
				sigs = append(sigs, fieldRef{soul, field, v, n.States[field]})
				continue
			}
			if err := checkField(soul, field, v, n.States[field]); err != nil {
				return fmt.Errorf("%w: %s.%s: %v", ErrUnverified, soul, field, err)
			}
		}
	}
	return verifyAll(sigs)
}

// fieldRef is one field of a graph.
type fieldRef struct {
	soul, field string
	v           Value
	state       float64
}

// signedField reports whether field of soul must carry its owner's
// signature: everything in a user's space but the account's own key.
func signedField(soul, field string) bool {
	if strings.HasPrefix(soul, "~@") {
		return false
	}
	pub := sea.PubOf(soul)
	return pub != "" && !(field == "pub" && soul == "~"+pub)
}

// verifyAll checks the signatures of fields, spread over the CPUs.
func verifyAll(fields []fieldRef) error {
	workers := min(len(fields), runtime.GOMAXPROCS(0))
	if workers <= 1 {
		for _, f := range fields {
			if err := checkField(f.soul, f.field, f.v, f.state); err != nil {
				return fmt.Errorf("%w: %s.%s: %v", ErrUnverified, f.soul, f.field, err)
			}
		}
		return nil
	}
	var (
		next   atomic.Int64
		failed atomic.Pointer[error]
		wg     sync.WaitGroup
	)
	for range workers {
		wg.Go(func() {
			for failed.Load() == nil {
				i := int(next.Add(1) - 1)
				if i >= len(fields) {
					return
				}
				f := fields[i]
				if err := checkField(f.soul, f.field, f.v, f.state); err != nil {
					err = fmt.Errorf("%w: %s.%s: %v", ErrUnverified, f.soul, f.field, err)
					failed.CompareAndSwap(nil, &err)
					return
				}
			}
		})
	}
	wg.Wait()
	if err := failed.Load(); err != nil {
		return *err
	}
	return nil
}

func checkField(soul, field string, v Value, state float64) error {
	switch {
	case soul == "~@": // the list of aliases: {"~@alice": {"#": "~@alice"}}
		if l, ok := v.(Link); !ok || l.Soul != "~@"+field {
			return errors.New("alias not same")
		}
	case strings.HasPrefix(soul, "~@"): // an alias's keys: {"~pub": {"#": "~pub"}}
		if l, ok := v.(Link); !ok || l.Soul != field {
			return errors.New("alias not same")
		}
	case sea.PubOf(soul) != "":
		pub := sea.PubOf(soul)
		if field == "pub" && soul == "~"+pub { // the account's own key, unsigned
			if v != String(pub) {
				return errors.New("account not same")
			}
			return nil
		}
		s, ok := v.(String)
		if !ok {
			return errors.New("unsigned data")
		}
		if _, err := sea.VerifyField(soul, field, string(s), state, pub); err != nil {
			return err
		}
	case strings.Contains(soul, "#"): // content addressed: the key is the hash
		want := field[strings.LastIndexByte(field, '#')+1:]
		var data any = json.RawMessage(lexical(v))
		if s, ok := v.(String); ok {
			data = string(s)
		}
		hash, err := sea.Hash(data)
		if err != nil {
			return err
		}
		if hash != want && hash != hexToBase64(want) {
			return errors.New("data hash not same as hash")
		}
	}
	return nil
}

// hexToBase64 is SEA's alternative spelling of a content hash.
func hexToBase64(h string) string {
	b, err := hex.DecodeString(h)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// unsign returns n with every signed value in a user's space replaced by
// the plain value inside, for reading. Signatures were checked when the
// data was written.
func unsign(n *Node) *Node {
	if n == nil || strings.HasPrefix(n.Soul, "~@") || sea.PubOf(n.Soul) == "" {
		return n
	}
	for k, v := range n.Fields {
		s, ok := v.(String)
		if !ok {
			continue
		}
		if plain, ok := signedValue(string(s)); ok {
			n.Fields[k] = plain
			continue
		}
		raw, ok := sea.FieldValue(string(s))
		if !ok {
			continue
		}
		if plain, err := decodeValue(raw); err == nil {
			n.Fields[k] = plain
		}
	}
	return n
}

// signedValue reads the value out of a stored signed field written in the
// form SEA writes it, {":":value,"~":"signature"}, without a JSON parser.
// Reads of user data do this for every field, every time. Anything not in
// exactly that form returns false, for sea.FieldValue to handle.
func signedValue(s string) (Value, bool) {
	const head, mid = `{":":`, `,"~":"`
	if !strings.HasPrefix(s, head) || !strings.HasSuffix(s, `"}`) {
		return nil, false
	}
	i := strings.LastIndex(s, mid)
	if i < len(head) || i+len(mid) > len(s)-2 {
		return nil, false
	}
	if sig := s[i+len(mid) : len(s)-2]; strings.ContainsAny(sig, `"\`) {
		return nil, false // the signature must be one plain string
	}
	val := s[len(head):i]
	if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' && plainString(val[1:len(val)-1]) {
		return String(val[1 : len(val)-1]), true
	}
	v, err := decodeValue(json.RawMessage(val))
	if err != nil {
		return nil, false
	}
	return v, true
}

// plainString reports whether s is the body of a JSON string that needs no
// unescaping: valid UTF-8, no quotes, backslashes or control characters.
func plainString(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == '"' || c == '\\' {
			return false
		}
	}
	return utf8.ValidString(s)
}
