// Package senderkey implements Signal-style sender keys for encrypted group
// messaging on top of SEA.
//
// Every member of a group has their own chain, a symmetric hash ratchet.
// Each message is encrypted with a fresh key derived from the chain, and the
// chain then moves forward one way, so the key for an old message can't be
// derived again. A member shares their chain (a Key) with each other member,
// sealed with SEA to that member's prekey:
//
//	s, _ := senderkey.NewSender()                  // alice's chain
//	sealed, _ := senderkey.Seal(s.Share(), bobPreKeyEPub)
//	msg, _ := s.Encrypt("hi team")                 // message 0
//
//	key, _ := senderkey.Open(sealed, bobPreKey)    // on bob's device
//	r := senderkey.NewReceiver(key)
//	plain, _ := r.Decrypt(msg)                     // `"hi team"`
//
// The package does only the crypto. It never touches the graph: where
// chains, sealed keys and messages are stored is up to the app (see the
// guide). Message encryption is plain SEA (sea.Encrypt with the message
// key), and sealing is SEA ECDH plus sea.Encrypt, so a GUN.js client that
// implements the same chain steps with WebCrypto can read the messages.
//
// Messages are not signed here. Store each member's messages in their own
// user space (~pub): peers reject writes there that the owner didn't sign,
// so a message read from ~alice was written by alice.
//
// Sender and Receiver hold secrets and change with every message. Keep them
// on the device (they marshal to JSON), never in the graph.
package senderkey

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"

	"ella.to/gundb/sea"
)

// MaxSkip is how many message keys a Receiver derives ahead to read a
// message that arrived before earlier ones. Further than that, Decrypt
// returns ErrTooFar.
const MaxSkip = 2000

// Errors returned by Decrypt.
var (
	ErrOld     = errors.New("senderkey: message key already used, or before this key was shared")
	ErrTooFar  = errors.New("senderkey: message too far ahead")
	ErrChain   = errors.New("senderkey: message is for another chain")
	ErrDecrypt = errors.New("senderkey: could not decrypt")
)

// info labels the message keys this package derives, so they can't be
// mistaken for keys derived for anything else.
const info = "gundb senderkey v1"

// Key is a chain at message N: whoever holds it can read message N and every
// later message of the chain, but none before N.
type Key struct {
	Chain string `json:"chain"` // random ID of the chain
	N     uint32 `json:"n"`     // the next message number
	Seed  []byte `json:"seed"`  // the chain key at N
}

// Message is one encrypted message. Data is SEA ciphertext.
type Message struct {
	Chain string `json:"chain"`
	N     uint32 `json:"n"`
	Data  string `json:"data"`
}

// Sender is a member's own chain, used to encrypt what they send.
type Sender struct {
	Key Key `json:"key"`
}

// NewSender starts a new chain with a random ID and chain key.
func NewSender() (*Sender, error) {
	id := make([]byte, 16)
	seed := make([]byte, 32)
	rand.Read(id)
	rand.Read(seed)
	return &Sender{Key: Key{Chain: base64.RawURLEncoding.EncodeToString(id), Seed: seed}}, nil
}

// Share returns the chain as it is now, to seal for another member. They
// will read the messages sent from now on, not the ones before.
func (s *Sender) Share() Key {
	k := s.Key
	k.Seed = slices.Clone(k.Seed)
	return k
}

// Encrypt encrypts data (a string, or any value, JSON-encoded, as
// sea.Encrypt takes it) with the next message key, then moves the chain
// forward and forgets that key.
func (s *Sender) Encrypt(data any) (Message, error) {
	if s.Key.N == math.MaxUint32 {
		return Message{}, errors.New("senderkey: chain is used up; start a new one")
	}
	mk, next := step(s.Key.Seed)
	ct, err := sea.Encrypt(data, mk)
	if err != nil {
		return Message{}, err
	}
	m := Message{Chain: s.Key.Chain, N: s.Key.N, Data: ct}
	clear(s.Key.Seed)
	s.Key.Seed, s.Key.N = next, s.Key.N+1
	return m, nil
}

// Receiver reads one other member's chain.
type Receiver struct {
	Key Key `json:"key"`
	// Skipped holds the keys of messages passed over to read a later one,
	// until those messages arrive. They are the only old keys kept.
	Skipped map[uint32]string `json:"skipped,omitempty"`
}

// NewReceiver reads the chain shared as k.
func NewReceiver(k Key) *Receiver {
	k.Seed = slices.Clone(k.Seed)
	return &Receiver{Key: k}
}

// Decrypt returns m's data as JSON, like sea.Decrypt. Each message can be
// decrypted once: its key is deleted afterwards, so keep the plain text if
// you need it again. A message from before the key was shared, or one
// already read, fails with ErrOld.
func (r *Receiver) Decrypt(m Message) (json.RawMessage, error) {
	if m.Chain != r.Key.Chain {
		return nil, ErrChain
	}
	if m.N < r.Key.N {
		mk, ok := r.Skipped[m.N]
		if !ok {
			return nil, ErrOld
		}
		plain, err := sea.Decrypt(m.Data, mk)
		if err != nil {
			return nil, ErrDecrypt
		}
		delete(r.Skipped, m.N)
		return plain, nil
	}
	if m.N-r.Key.N > MaxSkip {
		return nil, ErrTooFar
	}
	// Work on copies: nothing changes unless m decrypts.
	seed, skipped := r.Key.Seed, map[uint32]string{}
	for n := r.Key.N; n < m.N; n++ {
		skipped[n], seed = step(seed)
	}
	mk, next := step(seed)
	plain, err := sea.Decrypt(m.Data, mk)
	if err != nil {
		return nil, ErrDecrypt
	}
	if r.Skipped == nil && len(skipped) > 0 {
		r.Skipped = map[uint32]string{}
	}
	maps.Copy(r.Skipped, skipped)
	for len(r.Skipped) > MaxSkip { // drop the oldest
		delete(r.Skipped, slices.Min(slices.Collect(maps.Keys(r.Skipped))))
	}
	clear(r.Key.Seed)
	r.Key.Seed, r.Key.N = next, m.N+1
	return plain, nil
}

// step derives the message key for the chain key seed and the chain key
// after it, as Signal's sender keys do: HMAC(seed, 1) seeds the message key,
// HMAC(seed, 2) is the next chain key.
func step(seed []byte) (msgKey string, next []byte) {
	mac := hmac.New(sha256.New, seed)
	mac.Write([]byte{1})
	mkSeed := mac.Sum(nil)
	mac.Reset()
	mac.Write([]byte{2})
	next = mac.Sum(nil)
	mk, err := hkdf.Key(sha256.New, mkSeed, nil, info, 32)
	if err != nil {
		panic(err) // only for lengths sha256 can't produce
	}
	return base64.RawURLEncoding.EncodeToString(mk), next
}

// Sealed is a Key encrypted for one member's prekey.
type Sealed struct {
	PreKey string `json:"prekey"` // EPub of the prekey it is sealed to
	EPub   string `json:"epub"`   // public half of a one-off key
	Data   string `json:"data"`   // SEA ciphertext of the Key
}

// Seal encrypts k for the owner of the prekey with public key prekeyEPub.
// It uses a one-off key pair (SEA ECDH), so nothing but the prekey can open
// it: not even the sender's own SEA keys.
func Seal(k Key, prekeyEPub string) (Sealed, error) {
	eph, err := sea.NewPair()
	if err != nil {
		return Sealed{}, err
	}
	secret, err := sea.Secret(prekeyEPub, eph)
	if err != nil {
		return Sealed{}, err
	}
	data, err := sea.Encrypt(k, secret)
	if err != nil {
		return Sealed{}, err
	}
	return Sealed{PreKey: prekeyEPub, EPub: eph.EPub, Data: data}, nil
}

// Open decrypts a Sealed key with the prekey it was sealed to.
func Open(s Sealed, prekey *sea.Pair) (Key, error) {
	if s.PreKey != prekey.EPub {
		return Key{}, fmt.Errorf("senderkey: sealed to another prekey")
	}
	secret, err := sea.Secret(s.EPub, prekey)
	if err != nil {
		return Key{}, err
	}
	raw, err := sea.Decrypt(s.Data, secret)
	if err != nil {
		return Key{}, ErrDecrypt
	}
	var k Key
	if err := json.Unmarshal(raw, &k); err != nil || len(k.Seed) != 32 {
		return Key{}, ErrDecrypt
	}
	return k, nil
}
