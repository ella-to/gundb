package gundb

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"ella.to/gundb/sea"
)

// Errors returned by CreateUser and Login.
var (
	ErrUserExists = errors.New("gundb: user already created")
	ErrWrongLogin = errors.New("gundb: wrong user or password")
)

// User is a SEA identity: a key pair, and the part of the graph it owns,
// the node "~"+Pub and every soul under it ("~"+Pub+"/..."). Writes made
// through the user's refs are signed with its key; every peer verifies
// them and rejects writes to a user's space that the user did not sign.
// Anyone can read a user's space; use package sea to encrypt private data.
//
// Accounts are stored like gun.user() stores them, so a user created in Go
// can log in from GUN.js with the same alias and password, and back.
type User struct {
	db    *DB
	pair  *sea.Pair
	alias string
}

// CreateUser creates a user with a new key pair, like gun.user().create:
// the private keys are stored encrypted with a key derived from password,
// and alias points at the new public key. It returns ErrUserExists if a
// peer already knows the alias. As in GUN, that check is best effort: two
// peers that cannot see each other can create the same alias.
func (db *DB) CreateUser(ctx context.Context, alias, password string) (*User, error) {
	if alias == "" {
		return nil, errors.New("gundb: no user")
	}
	if len(password) < 8 {
		return nil, errors.New("gundb: password too short (8 characters minimum)")
	}
	rd := &reader{db: db, ctx: ctx, net: true}
	if n, err := rd.node("~@" + alias); err != nil {
		return nil, err
	} else if n != nil && len(n.Fields) > 0 {
		return nil, ErrUserExists
	}

	pair, err := sea.NewPair()
	if err != nil {
		return nil, err
	}
	salt := randomSalt()
	proof, err := sea.Work(password, salt)
	if err != nil {
		return nil, err
	}
	keys := `{"priv":` + jsQuote(pair.Priv) + `,"epriv":` + jsQuote(pair.EPriv) + `}`
	ek, err := sea.Encrypt(json.RawMessage(keys), proof)
	if err != nil {
		return nil, err
	}
	auth := `{"ek":` + strings.TrimPrefix(ek, "SEA") + `,"s":` + jsQuote(salt) + `}`

	u := &User{db: db, pair: pair, alias: alias}
	soul := "~" + pair.Pub
	state := db.state.Next()
	g := graph{}
	acct := g.node(soul)
	acct.Set("pub", String(pair.Pub), state)
	acct.Set("alias", String(alias), state)
	acct.Set("epub", String(pair.EPub), state)
	acct.Set("auth", String(auth), state)
	g.node("~@"+alias).Set(soul, Link{Soul: soul}, state)
	if err := u.sign(g); err != nil {
		return nil, err
	}
	if err := db.write(ctx, g, true); err != nil && !errors.Is(err, ErrNoPeers) {
		return nil, err
	}
	return u, nil
}

// Login finds alias and unlocks its keys with password, like
// gun.user().auth. It returns ErrWrongLogin if no account under that alias
// opens with the password.
func (db *DB) Login(ctx context.Context, alias, password string) (*User, error) {
	rd := &reader{db: db, ctx: ctx, net: true}
	pubs, err := rd.node("~@" + alias)
	if err != nil {
		return nil, err
	}
	if pubs == nil {
		return nil, ErrWrongLogin
	}
	for _, soul := range slices.Sorted(maps.Keys(pubs.Fields)) {
		if _, ok := pubs.Fields[soul].(Link); !ok {
			continue
		}
		acct, err := rd.node(soul)
		if err != nil {
			return nil, err
		}
		if pair := unlock(acct, password); pair != nil {
			return &User{db: db, pair: pair, alias: alias}, nil
		}
	}
	return nil, ErrWrongLogin
}

// unlock decrypts an account's private keys with password, or returns nil.
func unlock(acct *Node, password string) *sea.Pair {
	if acct == nil {
		return nil
	}
	pub, _ := acct.Fields["pub"].(String)
	epub, _ := acct.Fields["epub"].(String)
	authText, _ := acct.Fields["auth"].(String)
	var auth struct {
		EK json.RawMessage `json:"ek"`
		S  string          `json:"s"`
	}
	if pub == "" || json.Unmarshal([]byte(authText), &auth) != nil || auth.EK == nil {
		return nil
	}
	proof, err := sea.Work(password, auth.S)
	if err != nil {
		return nil
	}
	plain, err := sea.Decrypt(string(auth.EK), proof)
	if err != nil {
		return nil
	}
	var keys struct{ Priv, EPriv string }
	if json.Unmarshal(plain, &keys) != nil || keys.Priv == "" {
		return nil
	}
	return &sea.Pair{Pub: string(pub), Priv: keys.Priv, EPub: string(epub), EPriv: keys.EPriv}
}

// LoginPair uses an existing key pair, like gun.user().auth(pair).
func (db *DB) LoginPair(pair *sea.Pair) *User { return &User{db: db, pair: pair} }

// User returns a read-only ref to the space of the user with key pub, like
// gun.user(pub) in JS: db.User(pub).Get("profile").Once[Profile](ctx).
func (db *DB) User(pub string) *Ref { return db.Get("~" + pub) }

// Pub returns the user's public key, the ID of their space.
func (u *User) Pub() string { return u.pair.Pub }

// Alias returns the alias the user logged in with ("" with LoginPair).
func (u *User) Alias() string { return u.alias }

// Pair returns the user's key pair. Keep it secret: it signs as the user.
func (u *User) Pair() *sea.Pair { return u.pair }

// Root returns a ref to the user's own node, "~"+Pub. Puts through it, and
// through every ref derived from it, are signed by the user.
func (u *User) Root() *Ref { return &Ref{db: u.db, path: []string{"~" + u.pair.Pub}, user: u} }

// Get returns a ref to field key of the user's node, like
// gun.user().get(key) in JS. Puts through it are signed by the user.
func (u *User) Get(key string) *Ref { return u.Root().Get(key) }

// sign replaces every value in g that belongs to the user's space with a
// SEA signature over it. Nodes outside the user's space are left alone, so
// they go through the normal checks.
func (u *User) sign(g graph) error {
	for soul, n := range g {
		if strings.HasPrefix(soul, "~@") || sea.PubOf(soul) != u.pair.Pub {
			continue
		}
		for field, v := range n.Fields {
			if field == "pub" && soul == "~"+u.pair.Pub {
				continue // the account's key is stored as is
			}
			signed, err := sea.SignField(soul, field, json.RawMessage(lexical(v)), n.States[field], u.pair)
			if err != nil {
				return fmt.Errorf("gundb: sign %s.%s: %w", soul, field, err)
			}
			n.Fields[field] = String(signed)
		}
	}
	return nil
}

// randomSalt returns a 64-character random string, like String.random(64).
func randomSalt() string {
	b := make([]byte, 64)
	rand.Read(b)
	for i := range b {
		b[i] = idChars[int(b[i])%len(idChars)]
	}
	return string(b)
}
