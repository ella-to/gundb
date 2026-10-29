// Encrypted group chat with a shared group key. Only members can read it,
// and removing a member locks them out of what is said next.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"ella.to/gundb"
	"ella.to/gundb/sea"
)

// Message is what a member stores in their own feed: ciphertext, and the
// version of the group key it was encrypted with.
type Message struct {
	Version int    `json:"version"`
	Data    string `json:"data"`
}

// group is "~owner/groups/<id>" in the graph:
//
//	members/<pub>          true for each member (signed by the owner)
//	version                the current key version
//	keys/<version>/<pub>   the group key, encrypted for that member
//
// and each member writes their messages to "~member/groups/<id>/messages".
type group struct {
	db    *gundb.DB
	owner string // the owner's public key
	id    string
}

func main() {
	ctx := context.Background()
	// One DB stands in for the network: in a real app each person runs
	// their own peer, and only their own *gundb.User (their keys).
	db := gundb.New()
	defer db.Close()

	alice, bob, carol := newUser(ctx, db, "alice"), newUser(ctx, db, "bob"), newUser(ctx, db, "carol")
	eve := newUser(ctx, db, "eve") // never a member

	// Alice creates the group with bob and carol.
	g := &group{db: db, owner: alice.Pub(), id: "team"}
	check(g.setMembers(ctx, alice, alice, bob, carol))

	check(g.send(ctx, alice, "hi team"))
	check(g.send(ctx, bob, "hello!"))
	g.show(ctx, "carol", carol)

	// Everything is public in the graph, but eve has no key to read it.
	g.show(ctx, "eve", eve)

	// Alice removes carol: a new key version, given only to who is left.
	check(g.setMembers(ctx, alice, alice, bob))
	check(g.send(ctx, alice, "carol is gone"))
	g.show(ctx, "bob", bob)
	g.show(ctx, "carol", carol) // she still has the old key, not the new one
}

// setMembers makes members the group (owner included) and hands each of
// them a new group key, encrypted with a secret only the owner and that
// member can derive.
func (g *group) setMembers(ctx context.Context, owner *gundb.User, members ...*gundb.User) error {
	ref := owner.Get("groups").Get(g.id)
	version, _ := ref.Get("version").Once[int](ctx) // 0 for a new group
	version++

	key := make([]byte, 32)
	rand.Read(key)
	list := map[string]any{}
	old, _ := ref.Get("members").Once[map[string]bool](ctx)
	for pub := range old {
		list[pub] = nil // removed unless added back below
	}
	for _, m := range members {
		secret, err := sea.Secret(m.Pair().EPub, owner.Pair())
		if err != nil {
			return err
		}
		enc, err := sea.Encrypt(base64.StdEncoding.EncodeToString(key), secret)
		if err != nil {
			return err
		}
		if err := ref.Get("keys").Get(strconv.Itoa(version)).Get(m.Pub()).Put(ctx, enc); err != nil {
			return err
		}
		list[m.Pub()] = true
	}
	if err := ref.Get("members").Put(ctx, list); err != nil {
		return err
	}
	return ref.Get("version").Put(ctx, version) // last: members find their key first
}

// key returns the group key of version v for user u, decrypted with the
// secret u shares with the owner.
func (g *group) key(ctx context.Context, u *gundb.User, v int) (string, error) {
	enc, err := g.db.User(g.owner).Get("groups").Get(g.id).Get("keys").Get(strconv.Itoa(v)).Get(u.Pub()).Once[string](ctx)
	if err != nil {
		return "", err // no key for u: not a member at version v
	}
	ownerEPub, err := g.db.User(g.owner).Get("epub").Once[string](ctx)
	if err != nil {
		return "", err
	}
	secret, err := sea.Secret(ownerEPub, u.Pair())
	if err != nil {
		return "", err
	}
	raw, err := sea.Decrypt(enc, secret)
	if err != nil {
		return "", err
	}
	var key string
	return key, json.Unmarshal(raw, &key)
}

// send encrypts text with the current group key and adds it to u's feed.
func (g *group) send(ctx context.Context, u *gundb.User, text string) error {
	v, err := g.db.User(g.owner).Get("groups").Get(g.id).Get("version").Once[int](ctx)
	if err != nil {
		return err
	}
	key, err := g.key(ctx, u, v)
	if err != nil {
		return fmt.Errorf("%s is not a member: %w", u.Alias(), err)
	}
	enc, err := sea.Encrypt(text, key)
	if err != nil {
		return err
	}
	// A time-ordered key sorts the feed; the random part keeps keys unique.
	id := fmt.Sprintf("%020d-%s", time.Now().UnixNano(), rand.Text()[:6])
	return u.Get("groups").Get(g.id).Get("messages").Get(id).Put(ctx, Message{Version: v, Data: enc})
}

// show prints the conversation as u sees it: every member's feed, merged in
// time order, decrypted where u has the key.
func (g *group) show(ctx context.Context, name string, u *gundb.User) {
	fmt.Printf("--- %s sees:\n", name)
	members, _ := g.db.User(g.owner).Get("groups").Get(g.id).Get("members").Once[map[string]bool](ctx)
	type line struct{ id, from, text string }
	var lines []line
	keys := map[int]string{}
	for _, pub := range slices.Sorted(maps.Keys(members)) {
		from, _ := g.db.User(pub).Get("alias").Once[string](ctx)
		feed, _ := g.db.User(pub).Get("groups").Get(g.id).Get("messages").Once[map[string]Message](ctx)
		for id, m := range feed {
			text := "[can't read]"
			if _, ok := keys[m.Version]; !ok {
				keys[m.Version], _ = g.key(ctx, u, m.Version)
			}
			if raw, err := sea.Decrypt(m.Data, keys[m.Version]); err == nil {
				json.Unmarshal(raw, &text)
			}
			lines = append(lines, line{id, from, text})
		}
	}
	slices.SortFunc(lines, func(a, b line) int { return strings.Compare(a.id, b.id) })
	for _, l := range lines {
		fmt.Printf("%s: %s\n", l.from, l.text)
	}
}

func newUser(ctx context.Context, db *gundb.DB, alias string) *gundb.User {
	u, err := db.CreateUser(ctx, alias, alias+" password")
	check(err)
	return u
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
