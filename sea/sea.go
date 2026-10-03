// Package sea is GUN's Security, Encryption and Authorization layer
// (gun/sea.js), byte-compatible with GUN.js: data signed, encrypted or
// derived here can be verified, decrypted or rederived by GUN.js, and the
// other way around.
//
//	pair, _ := sea.NewPair()
//	signed, _ := sea.Sign("hello", pair)       // "SEA{\"m\":\"hello\",\"s\":...}"
//	msg, _ := sea.Verify(signed, pair.Pub)     // `"hello"`
//
//	secret, _ := sea.Secret(bob.EPub, alice)   // ECDH: Bob derives the same
//	enc, _ := sea.Encrypt("for bob", secret)
//	dec, _ := sea.Decrypt(enc, secret)         // `"for bob"`
//
// Values follow SEA's JavaScript conventions. Data to sign or encrypt is a
// string, used as is, or any other value, JSON-encoded. Verified and
// decrypted messages come back as JSON, and like SEA a string that holds
// JSON comes back parsed: Sign("123", pair) verifies to the number 123.
//
// Users, signed graph data and the checks a peer runs on it live in
// package gundb (DB.CreateUser, DB.Login); this package is the crypto.
package sea

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"ella.to/gundb/internal/js"
)

// Errors returned when SEA data does not check out.
var (
	ErrSignature = errors.New("sea: signature did not match")
	ErrDecrypt   = errors.New("sea: could not decrypt")
)

// Pair is a SEA key pair, as GUN.js stores it: a P-256 ECDSA key for
// signing (Pub, Priv) and a P-256 ECDH key for encryption (EPub, EPriv).
// Public keys are "x.y" and private keys "d", base64url without padding.
type Pair struct {
	Pub   string `json:"pub"`
	Priv  string `json:"priv,omitempty"`
	EPub  string `json:"epub"`
	EPriv string `json:"epriv,omitempty"`
}

// NewPair generates a new random key pair (SEA.pair).
func NewPair() (*Pair, error) {
	sk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	pub, err := sk.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	priv, err := sk.Bytes()
	if err != nil {
		return nil, err
	}
	ek, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Pair{
		Pub:   encodeXY(pub),
		Priv:  b64url.EncodeToString(priv),
		EPub:  encodeXY(ek.PublicKey().Bytes()),
		EPriv: b64url.EncodeToString(ek.Bytes()),
	}, nil
}

// Sign signs data with pair (SEA.sign) and returns "SEA{"m":...,"s":...}".
func Sign(data any, pair *Pair) (string, error) {
	m, err := message(data)
	if err != nil {
		return "", err
	}
	sig, err := sign(m.hashInput(), pair)
	if err != nil {
		return "", err
	}
	return `SEA{"m":` + m.json + `,"s":` + js.Quote(sig) + `}`, nil
}

// Verify checks that signed was signed by the key pub (SEA.verify) and
// returns the message as JSON. It returns ErrSignature if it was not.
func Verify(signed string, pub string) (json.RawMessage, error) {
	var env struct {
		M json.RawMessage `json:"m"`
		S string          `json:"s"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(signed, "SEA")), &env); err != nil || env.M == nil {
		return nil, fmt.Errorf("sea: not signed data")
	}
	m, err := parsed(env.M)
	if err != nil {
		return nil, err
	}
	if err := verify(m.hashInput(), env.S, pub); err != nil {
		return nil, err
	}
	return m.result()
}

// Encrypt encrypts data with key (SEA.encrypt): AES-GCM with a key derived
// from key and a random salt. key is a pair's EPriv, a shared Secret, or
// any passphrase, such as a Work proof.
func Encrypt(data any, key string) (string, error) {
	raw, err := encrypt(data, key)
	if err != nil {
		return "", err
	}
	return "SEA" + raw, nil
}

// Decrypt decrypts what Encrypt produced (SEA.decrypt) and returns the
// plain data as JSON. It returns ErrDecrypt for a wrong key or bad data.
func Decrypt(data string, key string) (json.RawMessage, error) {
	var env struct{ CT, IV, S string }
	if err := json.Unmarshal([]byte(strings.TrimPrefix(data, "SEA")), &env); err != nil || env.CT == "" {
		return nil, fmt.Errorf("sea: not encrypted data")
	}
	salt, err1 := b64std(env.S)
	iv, err2 := b64std(env.IV)
	ct, err3 := b64std(env.CT)
	if err := errors.Join(err1, err2, err3); err != nil || len(iv) == 0 {
		return nil, ErrDecrypt
	}
	gcm, err := aesGCM(key, salt, len(iv))
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, iv, ct, nil)
	if err != nil {
		return nil, ErrDecrypt
	}
	return textResult(string(plain))
}

// Secret derives the secret shared by pair and the owner of epub (SEA.secret,
// ECDH). Both sides get the same string; use it as an Encrypt key.
func Secret(epub string, pair *Pair) (string, error) {
	theirs, err := decodeXY(epub)
	if err != nil {
		return "", err
	}
	pk, err := ecdh.P256().NewPublicKey(theirs)
	if err != nil {
		return "", fmt.Errorf("sea: bad epub: %w", err)
	}
	d, err := b64urlDecode(pair.EPriv)
	if err != nil {
		return "", err
	}
	sk, err := ecdh.P256().NewPrivateKey(d)
	if err != nil {
		return "", fmt.Errorf("sea: bad epriv: %w", err)
	}
	shared, err := sk.ECDH(pk)
	if err != nil {
		return "", err
	}
	return b64url.EncodeToString(shared), nil // exported as a JWK "k"
}

// Work stretches data with salt using PBKDF2-SHA256, 100,000 rounds, 64
// bytes (SEA.work). GUN uses it to turn passwords into encryption keys.
func Work(data any, salt string) (string, error) {
	m, err := text(data)
	if err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, m, []byte(salt), 100_000, 64)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// Hash returns the base64 SHA-256 of data, as SEA.work(data, null, null,
// {name: "SHA-256"}) does. GUN uses it for content-addressed souls.
func Hash(data any) (string, error) {
	m, err := text(data)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(m))
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

// ---- messages ----

// msg is a value as SEA sees it after S.parse: a plain string, or JSON.
type msg struct {
	str   string // when isStr
	isStr bool
	json  string // the value as JSON.stringify writes it
}

// hashInput is what SEA hashes: a string as is, anything else as JSON.
func (m msg) hashInput() string {
	if m.isStr {
		return m.str
	}
	return m.json
}

// result is the verified message as JSON. Like S.parse, a string holding
// JSON is parsed once more.
func (m msg) result() (json.RawMessage, error) {
	if m.isStr {
		return textResult(m.str)
	}
	return json.RawMessage(m.json), nil
}

// message turns data into what SEA signs: S.parse(data).
func message(data any) (msg, error) {
	switch d := data.(type) {
	case string:
		if strings.HasPrefix(d, "SEA{") {
			d = d[3:]
		}
		if m, err := parsed([]byte(d)); err == nil {
			return m, nil
		}
		return msg{str: d, isStr: true, json: js.Quote(d)}, nil
	case json.RawMessage:
		return parsed(d)
	}
	b, err := json.Marshal(data)
	if err != nil {
		return msg{}, err
	}
	return parsed(b)
}

// parsed is a msg for the JSON value b.
func parsed(b []byte) (msg, error) {
	norm, err := js.Restringify(b)
	if err != nil {
		return msg{}, err
	}
	if norm[0] == '"' {
		var s string
		json.Unmarshal(norm, &s)
		return msg{str: s, isStr: true, json: string(norm)}, nil
	}
	return msg{json: string(norm)}, nil
}

// text is data as SEA.encrypt and SEA.work take it: a string as is,
// anything else as JSON.
func text(data any) (string, error) {
	if s, ok := data.(string); ok {
		return s, nil
	}
	var b []byte
	if r, ok := data.(json.RawMessage); ok {
		b = r
	} else {
		var err error
		if b, err = json.Marshal(data); err != nil {
			return "", err
		}
	}
	norm, err := js.Restringify(b)
	return string(norm), err
}

// textResult is S.parse(text) as JSON: parsed if it is JSON, else a string.
func textResult(t string) (json.RawMessage, error) {
	if norm, err := js.Restringify([]byte(t)); err == nil {
		return norm, nil
	}
	return json.RawMessage(js.Quote(t)), nil
}

// ---- crypto ----

func sign(data string, pair *Pair) (string, error) {
	if pair == nil || pair.Priv == "" {
		return "", errors.New("sea: no signing key")
	}
	d, err := b64urlDecode(pair.Priv)
	if err != nil {
		return "", err
	}
	sk, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d)
	if err != nil {
		return "", fmt.Errorf("sea: bad priv: %w", err)
	}
	digest := sigDigest(data)
	r, s, err := ecdsa.Sign(rand.Reader, sk, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64) // WebCrypto's IEEE P1363 form: r || s
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return base64.StdEncoding.EncodeToString(sig), nil
}

func verify(data, sig, pub string) error {
	xy, err := decodeXY(pub)
	if err != nil {
		return err
	}
	pk, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), xy)
	if err != nil {
		return fmt.Errorf("sea: bad pub: %w", err)
	}
	raw, err := b64std(sig)
	if err != nil || len(raw) != 64 {
		return ErrSignature
	}
	digest := sigDigest(data)
	r, s := new(big.Int).SetBytes(raw[:32]), new(big.Int).SetBytes(raw[32:])
	if !ecdsa.Verify(pk, digest[:], r, s) {
		return ErrSignature
	}
	return nil
}

// sigDigest is what SEA's ECDSA signatures cover: SEA hashes the data and
// hands the hash to WebCrypto's ECDSA-with-SHA-256, which hashes it again.
func sigDigest(data string) [32]byte {
	h := sha256.Sum256([]byte(data))
	return sha256.Sum256(h[:])
}

// encrypt returns the {"ct","iv","s"} object as JSON (SEA's raw form).
func encrypt(data any, key string) (string, error) {
	if key == "" {
		return "", errors.New("sea: no encryption key")
	}
	plain, err := text(data)
	if err != nil {
		return "", err
	}
	salt, iv := make([]byte, 9), make([]byte, 15) // SEA's sizes
	rand.Read(salt)
	rand.Read(iv)
	gcm, err := aesGCM(key, salt, len(iv))
	if err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, iv, []byte(plain), nil)
	enc := base64.StdEncoding.EncodeToString
	return `{"ct":` + js.Quote(enc(ct)) + `,"iv":` + js.Quote(enc(iv)) + `,"s":` + js.Quote(enc(salt)) + `}`, nil
}

// aesGCM derives SEA's AES-256 key for nonces of nonceSize bytes:
// SHA-256 of key + salt, where SEA's own Buffer turns the salt into a
// string one byte per character (String.fromCharCode, i.e. Latin-1).
func aesGCM(key string, salt []byte, nonceSize int) (cipher.AEAD, error) {
	k := sha256.Sum256([]byte(key + latin1(salt)))
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithNonceSize(block, nonceSize)
}

// ---- encodings ----

// latin1 maps every byte to the character with that code point.
func latin1(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

var b64url = base64.RawURLEncoding

// encodeXY turns an uncompressed point (0x04 || x || y) into "x.y".
func encodeXY(p []byte) string {
	return b64url.EncodeToString(p[1:33]) + "." + b64url.EncodeToString(p[33:65])
}

// decodeXY turns "x.y" into an uncompressed point.
func decodeXY(s string) ([]byte, error) {
	x, y, ok := strings.Cut(s, ".")
	if !ok {
		return nil, fmt.Errorf("sea: bad public key %q", s)
	}
	xb, err1 := b64urlDecode(x)
	yb, err2 := b64urlDecode(y)
	if err := errors.Join(err1, err2); err != nil || len(xb) > 32 || len(yb) > 32 {
		return nil, fmt.Errorf("sea: bad public key %q", s)
	}
	p := make([]byte, 65)
	p[0] = 4
	copy(p[33-len(xb):33], xb)
	copy(p[65-len(yb):], yb)
	return p, nil
}

func b64urlDecode(s string) ([]byte, error) {
	return b64url.DecodeString(strings.TrimRight(s, "="))
}

// b64std decodes base64 as leniently as Node's Buffer.from(s, 'base64'):
// either alphabet, padding optional.
func b64std(s string) ([]byte, error) {
	s = strings.NewReplacer("-", "+", "_", "/").Replace(strings.TrimRight(s, "="))
	return base64.RawStdEncoding.DecodeString(s)
}

// ---- graph data ----

// SignField signs one field of a node in a user's space the way SEA's put
// hook does: the signature covers the soul, field, value (as JSON) and HAM
// state. It returns the string GUN stores as the field's value,
// {":":value,"~":signature}.
func SignField(soul, field string, value json.RawMessage, state float64, pair *Pair) (string, error) {
	val, err := js.Restringify(value)
	if err != nil {
		return "", err
	}
	sig, err := sign(fieldPayload(soul, field, val, state), pair)
	if err != nil {
		return "", err
	}
	return `{":":` + string(val) + `,"~":` + js.Quote(sig) + `}`, nil
}

// VerifyField checks a stored field value made by SignField against the
// owner's key pub, and returns the plain value as JSON. Writes made by
// someone else under a SEA certificate are not supported and fail.
func VerifyField(soul, field, stored string, state float64, pub string) (json.RawMessage, error) {
	val, sig, err := splitField(stored)
	if err != nil {
		return nil, err
	}
	if err := verify(fieldPayload(soul, field, val, state), sig, pub); err != nil {
		return nil, err
	}
	return val, nil
}

// FieldValue returns the plain value inside a stored signed field without
// checking the signature (peers check it when the data arrives).
func FieldValue(stored string) (json.RawMessage, bool) {
	val, _, err := splitField(stored)
	return val, err == nil
}

func splitField(stored string) (val json.RawMessage, sig string, err error) {
	var f struct {
		V    json.RawMessage `json:":"`
		S    *string         `json:"~"`
		Cert json.RawMessage `json:"+"`
		By   json.RawMessage `json:"*"`
	}
	if json.Unmarshal([]byte(stored), &f) != nil || f.V == nil || f.S == nil {
		return nil, "", errors.New("sea: unsigned data")
	}
	if f.Cert != nil || f.By != nil {
		return nil, "", errors.New("sea: certificates are not supported")
	}
	if val, err = js.Restringify(f.V); err != nil {
		return nil, "", err
	}
	return val, *f.S, nil
}

// fieldPayload is JSON.stringify({"#": soul, ".": field, ":": value, ">": state}).
func fieldPayload(soul, field string, val json.RawMessage, state float64) string {
	return `{"#":` + js.Quote(soul) + `,".":` + js.Quote(field) + `,":":` + string(val) + `,">":` + js.Number(state) + `}`
}

// PubOf returns the public key a soul belongs to, or "" if it is not in a
// user's space: "~x.y" and "~x.y/profile" belong to "x.y" (SEA.opt.pub).
func PubOf(soul string) string {
	_, rest, ok := strings.Cut(soul, "~")
	if !ok {
		return ""
	}
	if i := strings.IndexByte(rest, '~'); i >= 0 {
		rest = rest[:i] // split('~')[1]
	}
	parts := splitKeep(rest)
	if len(parts) < 2 || strings.HasPrefix(parts[0], "@") {
		return ""
	}
	return parts[0] + "." + parts[1]
}

// splitKeep splits s on every non-word character like JS's split(/[^\w_-]/).
// JS works on UTF-16 code units, so a character above U+FFFF is two
// separators with an empty part between them.
func splitKeep(s string) []string {
	var parts []string
	start := 0
	for i, r := range s {
		if !isWord(r) {
			parts = append(parts, s[start:i])
			if r > 0xFFFF {
				parts = append(parts, "")
			}
			start = i + len(string(r))
		}
	}
	return append(parts, s[start:])
}

func isWord(r rune) bool {
	return r == '_' || r == '-' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9')
}
