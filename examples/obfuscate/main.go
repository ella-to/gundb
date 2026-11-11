// Hide the names, not only the values: a sealed scope stores tokens the
// relay cannot read or guess, and the key that reads them is shared with
// whoever you choose.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"

	"ella.to/gundb"
	"ella.to/gundb/sea"
	"ella.to/gundb/sea/pathmap"
)

type Account struct {
	Bank string `json:"bank"` // encrypted with the scope's key
	PIN  string `json:"pin"`
}

func main() {
	ctx := context.Background()
	relay := gundb.New()
	defer relay.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	go http.Serve(l, relay)
	url := "ws://" + l.Addr().String() + "/gun"
	alices := gundb.New(gundb.Options{Peers: []string{url}})
	bobs := gundb.New(gundb.Options{Peers: []string{url}})
	defer alices.Close()
	defer bobs.Close()

	alice, err := alices.CreateUser(ctx, "alice", "alice's password")
	check(err)
	bob, err := bobs.CreateUser(ctx, "bob", "bob's password")
	check(err)

	// Alice makes a scope: a key, and a root in her space that nothing
	// links to.
	key, err := pathmap.NewKey()
	check(err)
	root, err := pathmap.NewRoot(alice.Pub())
	check(err)
	vault := alice.Soul(root).Sealed(key) // signed by alice, names sealed

	bank, err := sea.Encrypt("IBAN DE89 3704 0044", key.ValueKey())
	check(err)
	check(vault.Get("accounts").Get("main").PutAck(ctx, Account{Bank: bank, PIN: "unset"}))

	// She gives bob a grant: the key and the root, encrypted with the
	// secret only the two of them share, under a field only they can name.
	secret, err := sea.Secret(bob.Pair().EPub, alice.Pair())
	check(err)
	grant, err := pathmap.SealGrant(pathmap.Grant{Key: key.Bytes(), Root: root, Label: "vault", Epoch: 1}, secret)
	check(err)
	grants := alice.Soul(pathmap.GrantsNode(alice.Pub()))
	check(grants.Get(pathmap.GrantField(secret, "vault")).PutAck(ctx, grant))

	// What the relay holds: tokens, and a link to a node named by tokens.
	stored, err := relay.Get(root).Node(ctx)
	check(err)
	for field, v := range stored.Fields {
		if l, ok := v.(gundb.Link); ok {
			fmt.Printf("relay: %s links to %s\n", field, l.Soul)
		}
	}

	// Bob works out the same field, opens his grant, and reads with the
	// real names.
	shared, err := sea.Secret(alice.Pair().EPub, bob.Pair())
	check(err)
	sealed, err := bobs.Get(pathmap.GrantsNode(alice.Pub())).Get(pathmap.GrantField(shared, "vault")).Once[string](ctx)
	check(err)
	g, err := pathmap.OpenGrant(sealed, shared)
	check(err)
	bobsKey, err := pathmap.KeyFrom(g.Key)
	check(err)
	acct, err := bobs.Get(g.Root).Sealed(bobsKey).Get("accounts").Get("main").Once[Account](ctx)
	check(err)
	iban, err := sea.Decrypt(acct.Bank, bobsKey.ValueKey())
	check(err)
	fmt.Println("bob reads accounts/main:", string(iban), "pin", acct.PIN)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
