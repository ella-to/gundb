// Package pathmap hides the names in a part of the graph: the keys of a
// path, and so the souls of the nodes under it. A relay, or anyone without
// the scope's key, sees tokens that mean nothing and cannot be guessed;
// whoever holds the key reads and writes with the real names.
//
// The key is the map: Seal turns a name into its token and Open turns the
// token back into the name, so no table has to be stored or kept in sync.
// A token is deterministic, so two devices that write the same name while
// offline write the same field, and HAM merges them as usual. It is bound
// to the node it is in, so one name gives unrelated tokens in two nodes.
//
//	key, _ := pathmap.NewKey()
//	tok := key.Seal("~alice/Qm3v", "bank")      // 43 characters of base64url
//	name, ok := key.Open("~alice/Qm3v", tok)    // "bank", true
//
// Sealing is SIV, as encrypted file systems such as gocryptfs seal file
// names: the IV is an HMAC-SHA256 of the soul and the padded name, which
// is then encrypted with AES-256-CTR. Open checks the HMAC, so a field
// sealed with another key, or not sealed at all, is reported as such.
// Names are padded to 16 bytes, which hides their length up to that step.
//
// A key reaches other people through grants: the key and the scope's root
// soul, encrypted with a secret the owner shares with each reader (SEA's
// ECDH), stored under a field only that reader can name (GrantField).
package pathmap

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"sync"

	"ella.to/gundb/sea"
)

// KeySize is the size of a scope's key.
const KeySize = 32

var b64 = base64.RawURLEncoding

// Key is the key of one scope. It is safe for concurrent use.
type Key struct {
	raw   []byte
	macs  sync.Pool    // HMACs keyed for the IV, reused: one costs several allocations
	block cipher.Block // AES-256, encrypts the padded name
	val   string       // the key values in the scope are encrypted with
}

// NewKey returns a new random key.
func NewKey() (*Key, error) {
	raw := make([]byte, KeySize)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	return KeyFrom(raw)
}

// KeyFrom returns the key whose bytes are raw, as Bytes returned them.
func KeyFrom(raw []byte) (*Key, error) {
	if len(raw) != KeySize {
		return nil, fmt.Errorf("pathmap: a key has %d bytes, not %d", KeySize, len(raw))
	}
	block, err := aes.NewCipher(derive(raw, "ctr"))
	if err != nil {
		return nil, err
	}
	k := &Key{
		raw:   append([]byte(nil), raw...),
		block: block,
		val:   base64.StdEncoding.EncodeToString(derive(raw, "val")),
	}
	mac := derive(raw, "mac")
	k.macs.New = func() any { return hmac.New(sha256.New, mac) }
	return k, nil
}

// derive returns the subkey of raw for purpose.
func derive(raw []byte, purpose string) []byte {
	m := hmac.New(sha256.New, raw)
	m.Write([]byte("gundb/pathmap/v1/" + purpose))
	return m.Sum(nil)
}

// Bytes returns the key's bytes, for KeyFrom. Keep them secret.
func (k *Key) Bytes() []byte { return append([]byte(nil), k.raw...) }

// ValueKey returns the key to encrypt the scope's values with, for
// sea.Encrypt and sea.Decrypt: everyone who holds the scope's key can
// read them.
func (k *Key) ValueKey() string { return k.val }

// Seal returns the token that stands for name in the node soul. Tokens use
// the base64url alphabet, so they hold no "/", "~", "#" or ".", which mean
// something in a soul. A name of up to 15 bytes gives 43 characters.
func (k *Key) Seal(soul, name string) string {
	p := pad(name)
	iv := k.iv(soul, p)
	out := make([]byte, aes.BlockSize+len(p))
	copy(out, iv)
	cipher.NewCTR(k.block, iv).XORKeyStream(out[aes.BlockSize:], p)
	return b64.EncodeToString(out)
}

// Open returns the name field stands for in the node soul. ok is false if
// field was not sealed with k for soul: another key, another node, or a
// name that is not sealed at all.
func (k *Key) Open(soul, field string) (name string, ok bool) {
	raw, err := b64.DecodeString(field)
	if err != nil || len(raw) < 2*aes.BlockSize || len(raw)%aes.BlockSize != 0 {
		return "", false
	}
	iv, ct := raw[:aes.BlockSize], raw[aes.BlockSize:]
	p := make([]byte, len(ct))
	cipher.NewCTR(k.block, iv).XORKeyStream(p, ct)
	if subtle.ConstantTimeCompare(k.iv(soul, p), iv) != 1 {
		return "", false
	}
	return unpad(p)
}

// iv is the synthetic IV: the first 16 bytes of HMAC(soul, 0, padded name).
func (k *Key) iv(soul string, p []byte) []byte {
	m := k.macs.Get().(hash.Hash)
	defer k.macs.Put(m)
	m.Reset()
	io.WriteString(m, soul)
	m.Write([]byte{0})
	m.Write(p)
	var sum [sha256.Size]byte
	return append([]byte(nil), m.Sum(sum[:0])[:aes.BlockSize]...)
}

// pad pads name to a multiple of 16 bytes, PKCS #7 style: n bytes of n.
func pad(name string) []byte {
	n := aes.BlockSize - len(name)%aes.BlockSize
	p := make([]byte, len(name)+n)
	copy(p, name)
	for i := len(name); i < len(p); i++ {
		p[i] = byte(n)
	}
	return p
}

func unpad(p []byte) (string, bool) {
	n := int(p[len(p)-1])
	if n == 0 || n > aes.BlockSize {
		return "", false
	}
	for _, b := range p[len(p)-n:] {
		if int(b) != n {
			return "", false
		}
	}
	return string(p[:len(p)-n]), true
}

// NewRoot returns a new random root soul for a scope in the space of the
// user with public key pub: ~<pub>/<22 random characters>.
func NewRoot(pub string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "~" + pub + "/" + b64.EncodeToString(b), nil
}

// GrantsNode returns the soul of the node that holds the grants of the
// user with public key pub.
func GrantsNode(pub string) string { return "~" + pub + "/pathmap" }

// Grant is what a reader learns from a grant: how to read one scope.
type Grant struct {
	Key   []byte `json:"k"`
	Root  string `json:"soul"`  // the scope's root node
	Label string `json:"label"` // the name the owner gave the scope
	Epoch int    `json:"epoch"` // 1, then one more on every new key
	// Readers, in the owner's own grant only, are the public keys of the
	// users the owner gave a grant to. Grant fields can't be listed back,
	// by design, so the owner keeps the list here.
	Readers []string `json:"readers,omitempty"`
}

// GrantField returns the field, in the owner's grants node, of the grant
// for the scope label sealed with secret: the secret the owner and the
// reader share (sea.Secret). Only the two of them can work it out, so a
// reader finds their grant with one read, and nobody else learns who has
// one.
func GrantField(secret, label string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("gundb/pathmap/v1/grant"))
	m.Write([]byte{0})
	m.Write([]byte(label))
	return b64.EncodeToString(m.Sum(nil))[:22]
}

// SealGrant encrypts g with secret, as stored under GrantField.
func SealGrant(g Grant, secret string) (string, error) {
	b, err := json.Marshal(g)
	if err != nil {
		return "", err
	}
	return sea.Encrypt(json.RawMessage(b), secret)
}

// ErrNoGrant is returned by OpenGrant when sealed is not a grant secret
// opens.
var ErrNoGrant = errors.New("pathmap: not a grant for this secret")

// OpenGrant decrypts a grant SealGrant made with secret.
func OpenGrant(sealed, secret string) (Grant, error) {
	var g Grant
	if !strings.HasPrefix(sealed, "SEA{") {
		return g, ErrNoGrant
	}
	plain, err := sea.Decrypt(sealed, secret)
	if err != nil {
		return g, ErrNoGrant
	}
	if err := json.Unmarshal(plain, &g); err != nil || len(g.Key) != KeySize || g.Root == "" {
		return Grant{}, ErrNoGrant
	}
	return g, nil
}
