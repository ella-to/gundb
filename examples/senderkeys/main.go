// Encrypted group chat with sender keys (a ratchet): every message has its
// own key, new members can't read history, removed members can't read what
// comes next, and a stolen device can't read what it already read.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"ella.to/gundb"
	"ella.to/gundb/sea"
	"ella.to/gundb/sea/senderkey"
)

// In the graph (all public, all signed by whoever owns the space):
//
//	~owner/groups/<id>/members/<pub>          true for each member
//	~member/prekey                            the member's current prekey (an epub)
//	~member/groups/<id>/chain                 the ID of the member's current chain
//	~member/groups/<id>/keys/<chain>/<pub>    member's chain, sealed for <pub>
//	~member/groups/<id>/messages/<msgID>      a senderkey.Message
//
// On each device, never in the graph: prekeys, chains, and read messages.
type member struct {
	*gundb.User
	name    string
	db      *gundb.DB
	prekeys map[string]*sea.Pair           // by epub
	sender  *senderkey.Sender              // my chain
	chains  map[string]*senderkey.Receiver // other members' chains, by ID
	seen    map[string]bool                // message IDs already handled
}

type group struct {
	owner string // the owner's public key
	id    string
}

func main() {
	ctx := context.Background()
	// One DB stands in for the network: in a real app each person runs
	// their own peer, and each member value is their own device.
	db := gundb.New()
	defer db.Close()
	alice, bob, carol, dave := join(ctx, db, "alice"), join(ctx, db, "bob"), join(ctx, db, "carol"), join(ctx, db, "dave")

	// Alice creates the group. Each member starts a chain and shares it.
	g := group{owner: alice.Pub(), id: "team"}
	check(alice.setMembers(ctx, g, alice, bob, carol))
	for _, m := range []*member{alice, bob, carol} {
		check(m.newChain(ctx, g))
	}
	check(alice.send(ctx, g, "hi team"))
	check(bob.send(ctx, g, "hello!"))
	carol.read(ctx, g)

	// Dave joins. The others share their chains as they are now, so dave
	// reads from here on but not the history: chains only move forward.
	check(alice.setMembers(ctx, g, alice, bob, carol, dave))
	check(dave.newChain(ctx, g))
	for _, m := range []*member{alice, bob, carol} {
		check(m.share(ctx, g, m.sender, dave.Pub()))
	}
	check(alice.send(ctx, g, "welcome dave"))
	dave.read(ctx, g)

	// Carol leaves. Everyone left starts a new chain that carol never gets.
	check(alice.setMembers(ctx, g, alice, bob, dave))
	for _, m := range []*member{alice, bob, dave} {
		check(m.newChain(ctx, g))
	}
	check(bob.send(ctx, g, "carol can't read this"))
	carol.read(ctx, g)
	bob.read(ctx, g)

	// read opened every new chain sealed for bob, so he can now replace
	// his prekey and delete the old one. He still reads what comes next.
	check(bob.newPrekey(ctx))
	check(alice.send(ctx, g, "still private"))
	bob.read(ctx, g)

	// A thief with bob's password and a copy of his device can't read
	// anything bob already read: his chains have moved past those
	// messages, and the sealed keys in the graph need the deleted prekey.
	thief := bob.stolen("thief with bob's device")
	thief.read(ctx, g)

	// Bob's password alone (his SEA keys, on a new device) opens nothing:
	// chains are sealed to prekeys, which never leave the device.
	login, err := db.Login(ctx, "bob", "bob password")
	check(err)
	fresh := &member{User: login, name: "bob's password on a new device", db: db, prekeys: map[string]*sea.Pair{}, chains: map[string]*senderkey.Receiver{}, seen: map[string]bool{}}
	fresh.read(ctx, g)
}

// join creates a user and publishes their first prekey.
func join(ctx context.Context, db *gundb.DB, alias string) *member {
	u, err := db.CreateUser(ctx, alias, alias+" password")
	check(err)
	m := &member{User: u, name: alias, db: db, prekeys: map[string]*sea.Pair{}, chains: map[string]*senderkey.Receiver{}, seen: map[string]bool{}}
	check(m.newPrekey(ctx))
	return m
}

// newPrekey publishes a new prekey for others to seal chains to, and
// deletes the old ones. Do it once the chains sealed to them are open.
func (m *member) newPrekey(ctx context.Context) error {
	p, err := sea.NewPair()
	if err != nil {
		return err
	}
	clear(m.prekeys)
	m.prekeys[p.EPub] = p
	return m.Get("prekey").Put(ctx, p.EPub)
}

// setMembers replaces the member list (only the owner can write it).
func (m *member) setMembers(ctx context.Context, g group, members ...*member) error {
	ref := m.Get("groups").Get(g.id).Get("members")
	list := map[string]any{}
	old, _ := ref.Once[map[string]bool](ctx)
	for pub := range old {
		list[pub] = nil
	}
	for _, x := range members {
		list[x.Pub()] = true
	}
	return ref.Put(ctx, list)
}

func (g group) members(ctx context.Context, db *gundb.DB) []string {
	list, _ := db.User(g.owner).Get("groups").Get(g.id).Get("members").Once[map[string]bool](ctx)
	return slices.Sorted(maps.Keys(list))
}

// newChain starts a new chain and shares it with every other member.
func (m *member) newChain(ctx context.Context, g group) error {
	s, err := senderkey.NewSender()
	if err != nil {
		return err
	}
	m.sender = s
	if err := m.Get("groups").Get(g.id).Get("chain").Put(ctx, s.Key.Chain); err != nil {
		return err
	}
	var others []string
	for _, pub := range g.members(ctx, m.db) {
		if pub != m.Pub() {
			others = append(others, pub)
		}
	}
	return m.share(ctx, g, s, others...)
}

// share seals s, as it is now, to the current prekey of each member in to.
func (m *member) share(ctx context.Context, g group, s *senderkey.Sender, to ...string) error {
	for _, pub := range to {
		prekey, err := m.db.User(pub).Get("prekey").Once[string](ctx)
		if err != nil {
			return err
		}
		sealed, err := senderkey.Seal(s.Share(), prekey)
		if err != nil {
			return err
		}
		if err := m.Get("groups").Get(g.id).Get("keys").Get(s.Key.Chain).Get(pub).Put(ctx, sealed); err != nil {
			return err
		}
	}
	return nil
}

// send encrypts text with the next key of m's chain and adds it to m's feed.
func (m *member) send(ctx context.Context, g group, text string) error {
	msg, err := m.sender.Encrypt(text)
	if err != nil {
		return err
	}
	id := fmt.Sprintf("%020d-%s", time.Now().UnixNano(), rand.Text()[:6])
	m.seen[id] = true
	return m.Get("groups").Get(g.id).Get("messages").Get(id).Put(ctx, msg)
}

// read opens the other members' new chains, then prints the messages m
// hasn't seen yet from their feeds, in time order. Each message can be
// decrypted only once, so a real app stores the plain text on the device.
func (m *member) read(ctx context.Context, g group) {
	type line struct{ id, text string }
	var lines []line
	for _, pub := range g.members(ctx, m.db) {
		if pub == m.Pub() {
			continue
		}
		if chain, err := m.db.User(pub).Get("groups").Get(g.id).Get("chain").Once[string](ctx); err == nil && m.chains[chain] == nil {
			m.open(ctx, g, pub, chain) // open it now, while we have the prekey
		}
		from, _ := m.db.User(pub).Get("alias").Once[string](ctx)
		feed, _ := m.db.User(pub).Get("groups").Get(g.id).Get("messages").Once[map[string]senderkey.Message](ctx)
		for _, id := range slices.Sorted(maps.Keys(feed)) {
			if m.seen[id] {
				continue
			}
			m.seen[id] = true
			lines = append(lines, line{id, from + ": " + m.decrypt(ctx, g, pub, feed[id])})
		}
	}
	slices.SortFunc(lines, func(a, b line) int { return strings.Compare(a.id, b.id) })
	fmt.Printf("--- %s reads:\n", m.name)
	for _, l := range lines {
		fmt.Println(l.text)
	}
}

// open finds from's chain sealed for m and opens it with m's prekey.
func (m *member) open(ctx context.Context, g group, from, chain string) error {
	sealed, err := m.db.User(from).Get("groups").Get(g.id).Get("keys").Get(chain).Get(m.Pub()).Once[senderkey.Sealed](ctx)
	if err != nil {
		return errors.New("no key for me")
	}
	prekey, ok := m.prekeys[sealed.PreKey]
	if !ok {
		return errors.New("sealed to a prekey I don't have")
	}
	key, err := senderkey.Open(sealed, prekey)
	if err != nil {
		return err
	}
	m.chains[chain] = senderkey.NewReceiver(key)
	return nil
}

func (m *member) decrypt(ctx context.Context, g group, from string, msg senderkey.Message) string {
	r, ok := m.chains[msg.Chain]
	if !ok {
		if err := m.open(ctx, g, from, msg.Chain); err != nil {
			return "[can't read: " + err.Error() + "]"
		}
		r = m.chains[msg.Chain]
	}
	raw, err := r.Decrypt(msg)
	if errors.Is(err, senderkey.ErrOld) {
		return "[can't read: before my key, or already read]"
	} else if err != nil {
		return "[can't read: " + err.Error() + "]"
	}
	var text string
	json.Unmarshal(raw, &text)
	return text
}

// stolen returns a copy of m's device as it is saved now: its prekeys and
// chains (round-tripped through JSON, as a device would store them), with
// no record of which messages it has read.
func (m *member) stolen(name string) *member {
	saved, err := json.Marshal(struct {
		Prekeys map[string]*sea.Pair
		Chains  map[string]*senderkey.Receiver
	}{m.prekeys, m.chains})
	check(err)
	t := &member{User: m.User, name: name, db: m.db, seen: map[string]bool{}}
	check(json.Unmarshal(saved, &struct {
		Prekeys *map[string]*sea.Pair
		Chains  *map[string]*senderkey.Receiver
	}{&t.prekeys, &t.chains}))
	return t
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
