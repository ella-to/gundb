package gundb

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"ella.to/gundb/sea"
)

type Profile struct {
	Name string `json:"name"`
	City string `json:"city,omitempty"`
}

func TestCreateUserAndLogin(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	if _, err := db.CreateUser(ctx, "ali", "short"); err == nil {
		t.Fatal("a 5 character password was accepted")
	}
	ali, err := db.CreateUser(ctx, "ali", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser(ctx, "ali", "another pass"); !errors.Is(err, ErrUserExists) {
		t.Fatalf("second create: %v", err)
	}
	if _, err := db.Login(ctx, "ali", "wrong password"); !errors.Is(err, ErrWrongLogin) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := db.Login(ctx, "nobody", "correct horse"); !errors.Is(err, ErrWrongLogin) {
		t.Fatalf("unknown alias: %v", err)
	}
	again, err := db.Login(ctx, "ali", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if *again.Pair() != *ali.Pair() || again.Alias() != "ali" {
		t.Fatalf("login gave other keys: %+v", again.Pair())
	}
	alias, err := db.User(ali.Pub()).Get("alias").Once[string](ctx)
	if err != nil || alias != "ali" {
		t.Fatalf("account alias = %q, %v", alias, err)
	}
}

// A user created on one peer logs in on another, through a relay, and
// everyone reads their signed data as plain values.
func TestUsersAcrossPeers(t *testing.T) {
	ctx := t.Context()
	relay, a, b := newDB(t), newDB(t), newDB(t)
	link(t, relay, a)
	link(t, relay, b)

	ali, err := a.CreateUser(ctx, "ali", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if err := ali.Get("profile").PutAck(ctx, Profile{Name: "Ali", City: "Toronto"}); err != nil {
		t.Fatal(err)
	}
	// On b, nothing is cached: the account comes from the relay.
	again, err := b.Login(ctx, "ali", "correct horse")
	if err != nil {
		t.Fatalf("login on another peer: %v", err)
	}
	if again.Pub() != ali.Pub() {
		t.Fatal("other keys")
	}
	p, err := b.User(ali.Pub()).Get("profile").Once[Profile](ctx)
	if err != nil || p != (Profile{Name: "Ali", City: "Toronto"}) {
		t.Fatalf("profile = %+v, %v", p, err)
	}
	// The relay stores what GUN stores: signed values.
	n, _ := relay.store.Get(ctx, "~"+ali.Pub()+"/profile")
	if s, _ := n.Fields["name"].(String); !strings.HasPrefix(string(s), `{":":"Ali","~":"`) {
		t.Fatalf("relay stored %q", s)
	}
}

func TestForgedUserDataIsRejected(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	ali, err := db.CreateUser(ctx, "ali", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := db.CreateUser(ctx, "bob", "battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if err := ali.Get("name").Put(ctx, "Ali"); err != nil {
		t.Fatal(err)
	}
	space := db.User(ali.Pub())

	// Unsigned, through a ref without a user.
	if err := space.Get("name").Put(ctx, "Mallory"); !errors.Is(err, ErrUnverified) {
		t.Fatalf("unsigned write: %v", err)
	}
	// Signed, but by bob.
	g := graph{}
	g.node("~"+ali.Pub()).Set("name", String("Mallory"), db.state.Next())
	signed, _ := sea.SignField("~"+ali.Pub(), "name", json.RawMessage(`"Mallory"`), g["~"+ali.Pub()].States["name"], bob.Pair())
	g["~"+ali.Pub()].Fields["name"] = String(signed)
	if _, err := db.apply(ctx, g); !errors.Is(err, ErrUnverified) {
		t.Fatalf("write signed by another user: %v", err)
	}

	// Ali's real signature on "name", replayed onto "nick".
	n, _ := db.store.Get(ctx, "~"+ali.Pub())
	g = graph{}
	g.node("~"+ali.Pub()).Set("nick", n.Fields["name"], n.States["name"])
	if _, err := db.apply(ctx, g); !errors.Is(err, ErrUnverified) {
		t.Fatalf("replayed signature: %v", err)
	}
	// The account's pub must be its own key.
	g = graph{}
	g.node("~"+ali.Pub()).Set("pub", String(bob.Pub()), db.state.Next())
	if _, err := db.apply(ctx, g); !errors.Is(err, ErrUnverified) {
		t.Fatalf("swapped pub: %v", err)
	}
	if name, _ := space.Get("name").Once[string](ctx); name != "Ali" {
		t.Fatalf("name is %q after the attacks", name)
	}
}

func TestAliasRules(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	state := db.state.Next()
	for _, c := range []struct {
		name        string
		soul, field string
		v           Value
		ok          bool
	}{
		{"alias list entry", "~@", "ali", Link{Soul: "~@ali"}, true},
		{"alias list pointing elsewhere", "~@", "ali", Link{Soul: "~@bob"}, false},
		{"alias key", "~@ali", "~x.y", Link{Soul: "~x.y"}, true},
		{"alias key pointing elsewhere", "~@ali", "~x.y", Link{Soul: "~evil.key"}, false},
		{"alias key that is not a link", "~@ali", "~x.y", String("~x.y"), false},
	} {
		g := graph{}
		g.node(c.soul).Set(c.field, c.v, state)
		_, err := db.apply(ctx, g)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}

func TestContentAddressedSouls(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	hash, _ := sea.Hash("hello")
	if err := db.Get("#").Get(hash).Put(ctx, "hello"); err != nil {
		t.Fatalf("content under its hash: %v", err)
	}
	if err := db.Get("#").Get(hash).Put(ctx, "changed"); !errors.Is(err, ErrUnverified) {
		t.Fatalf("content under another hash: %v", err)
	}
}

func TestSetInUserSpace(t *testing.T) {
	ctx := t.Context()
	db := newDB(t)
	ali, err := db.CreateUser(ctx, "ali", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	item, err := ali.Get("posts").Set(ctx, Profile{Name: "first post"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(item.Key(), "~"+ali.Pub()+"/") {
		t.Fatalf("item soul %q is outside the user's space", item.Key())
	}
	posts, err := db.User(ali.Pub()).Get("posts").Once[map[string]Profile](ctx)
	if err != nil || len(posts) != 1 || posts[item.Key()].Name != "first post" {
		t.Fatalf("posts = %+v, %v", posts, err)
	}
}

// A peer cannot get forged user data stored or relayed over the wire.
func TestForgedUserDataOverTheWire(t *testing.T) {
	ctx := t.Context()
	relay := newDB(t)
	ali, err := relay.CreateUser(ctx, "ali", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	conn := newScriptConn()
	go relay.AttachAs(conn, "")
	eventually(t, func() bool { return len(relay.Peers()) == 1 })
	soul := "~" + ali.Pub()
	conn.recv <- []byte(`{"#":"forge1","put":{"` + soul + `":{"_":{"#":"` + soul + `",">":{"name":1}},"name":"Mallory"}}}`)

	deadline := time.After(3 * time.Second)
	for {
		select {
		case frame := <-conn.sent:
			if strings.Contains(string(frame), `"@":"forge1"`) {
				if !strings.Contains(string(frame), "unverified") {
					t.Fatalf("forged put acked with %s", frame)
				}
				if n, _ := relay.store.Get(ctx, soul); n != nil && n.Fields["name"] != nil {
					t.Fatalf("relay stored %+v", n)
				}
				return
			}
		case <-deadline:
			t.Fatal("no answer to the forged put")
		}
	}
}
