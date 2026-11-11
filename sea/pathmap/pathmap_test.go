package pathmap

import (
	"bytes"
	"strings"
	"testing"

	"ella.to/gundb/sea"
)

func testKey(t *testing.T, fill byte) *Key {
	t.Helper()
	k, err := KeyFrom(bytes.Repeat([]byte{fill}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealOpen(t *testing.T) {
	k := testKey(t, 1)
	for _, name := range []string{"", "a", "bank", "fifteen bytes!!", "sixteen bytes!!!", "ünïcødé/with~signs#and.dots", strings.Repeat("x", 100)} {
		tok := k.Seal("~pub/root", name)
		if got, ok := k.Open("~pub/root", tok); !ok || got != name {
			t.Fatalf("%q: opened %q, %v", name, got, ok)
		}
		if strings.ContainsAny(tok, "/~#.+=") {
			t.Fatalf("%q: token %q holds a character that means something in a soul", name, tok)
		}
		if k.Seal("~pub/root", name) != tok {
			t.Fatalf("%q: not deterministic", name)
		}
	}
	if n := len(k.Seal("s", "fifteen bytes!!")); n != 43 {
		t.Fatalf("a 15-byte name gives %d characters, want 43", n)
	}
	if len(k.Seal("s", "a")) != len(k.Seal("s", "fifteen bytes!!")) {
		t.Fatal("name length shows through the padding")
	}
}

func TestOpenRefuses(t *testing.T) {
	k, other := testKey(t, 1), testKey(t, 2)
	tok := k.Seal("~pub/root", "bank")
	for _, tc := range []struct {
		name  string
		key   *Key
		soul  string
		field string
	}{
		{"another key", other, "~pub/root", tok},
		{"another node", k, "~pub/other", tok},
		{"a plain name", k, "~pub/root", "bank"},
		{"a plain name of token length", k, "~pub/root", strings.Repeat("A", 43)},
		{"cut short", k, "~pub/root", tok[:30]},
		{"one character changed", k, "~pub/root", tok[:10] + string(tok[10]^1) + tok[11:]},
		{"not base64", k, "~pub/root", "!!!"},
	} {
		if name, ok := tc.key.Open(tc.soul, tc.field); ok {
			t.Fatalf("%s: opened as %q", tc.name, name)
		}
	}
}

func TestBoundToNode(t *testing.T) {
	k := testKey(t, 1)
	if k.Seal("~pub/a", "title") == k.Seal("~pub/b", "title") {
		t.Fatal("one name gives one token in two nodes")
	}
}

// TestVector pins the construction, for implementations in other
// languages (a GUN.js helper must give the same tokens). The values were
// also worked out with Node's crypto module, independently of this code.
func TestVector(t *testing.T) {
	k := testKey(t, 0x42)
	if got := k.Seal("~pub/root", "bank"); got != vectorToken {
		t.Fatalf("Seal = %q, want %q", got, vectorToken)
	}
	if got := GrantField("shared secret", "vault"); got != vectorGrant {
		t.Fatalf("GrantField = %q, want %q", got, vectorGrant)
	}
}

const (
	vectorToken = "TpTrSv9Xfar7wxO61NBgSGuRxA5ePjOd5nQfgN2_GZU"
	vectorGrant = "lksf9Wga-GTMj0aYbVe1el"
)

func TestGrants(t *testing.T) {
	alice, _ := sea.NewPair()
	bob, _ := sea.NewPair()
	carol, _ := sea.NewPair()
	ab, _ := sea.Secret(bob.EPub, alice)
	ba, _ := sea.Secret(alice.EPub, bob)
	ac, _ := sea.Secret(carol.EPub, alice)
	if GrantField(ab, "vault") != GrantField(ba, "vault") {
		t.Fatal("alice and bob work out different grant fields")
	}
	if GrantField(ab, "vault") == GrantField(ac, "vault") || GrantField(ab, "vault") == GrantField(ab, "notes") {
		t.Fatal("grant fields collide")
	}
	k, _ := NewKey()
	root, _ := NewRoot(alice.Pub)
	if !strings.HasPrefix(root, "~"+alice.Pub+"/") || len(root) != len("~"+alice.Pub+"/")+22 {
		t.Fatalf("root %q", root)
	}
	sealed, err := SealGrant(Grant{Key: k.Bytes(), Root: root, Label: "vault", Epoch: 1}, ab)
	if err != nil {
		t.Fatal(err)
	}
	g, err := OpenGrant(sealed, ba)
	if err != nil || g.Root != root || g.Label != "vault" || !bytes.Equal(g.Key, k.Bytes()) {
		t.Fatalf("bob opened %+v, %v", g, err)
	}
	if _, err := OpenGrant(sealed, ac); err != ErrNoGrant {
		t.Fatalf("carol opened bob's grant: %v", err)
	}
	if _, err := OpenGrant("not a grant", ab); err != ErrNoGrant {
		t.Fatalf("garbage opened: %v", err)
	}
}

func TestValueKey(t *testing.T) {
	k := testKey(t, 1)
	enc, err := sea.Encrypt("IBAN DE89", k.ValueKey())
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := KeyFrom(k.Bytes())
	if plain, err := sea.Decrypt(enc, k2.ValueKey()); err != nil || string(plain) != `"IBAN DE89"` {
		t.Fatalf("decrypted %s, %v", plain, err)
	}
	if testKey(t, 2).ValueKey() == k.ValueKey() {
		t.Fatal("two keys share a value key")
	}
}
