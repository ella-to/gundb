package gundb

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"ella.to/gundb/sea"
)

// ErrUnverified is returned when a write breaks SEA's rules: unsigned or
// badly signed data in a user's space (~pub), a forged alias (~@alias), or
// content that does not match its hash (souls with #).
var ErrUnverified = errors.New("gundb: SEA: unverified data")

// checkSEA applies the checks every SEA peer runs on incoming data
// (gun/sea.js, check). Every DB runs them on every write, local or remote,
// like GUN.js peers do, so forged user data is never stored or relayed.
func checkSEA(g graph) error {
	for soul, n := range g {
		for field, v := range n.Fields {
			if err := checkField(soul, field, v, n.States[field]); err != nil {
				return fmt.Errorf("%w: %s.%s: %v", ErrUnverified, soul, field, err)
			}
		}
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
