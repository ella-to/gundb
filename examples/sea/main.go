// SEA users: signed data only its owner can write, and encrypted messages
// only the recipient can read. Compatible with gun.user() in GUN.js.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"

	"ella.to/gundb"
	"ella.to/gundb/sea"
)

type Profile struct {
	Name string `json:"name"`
}

func main() {
	ctx := context.Background()

	// A relay and three devices. No rules to configure: every peer checks
	// SEA signatures on every write.
	relay := gundb.New()
	defer relay.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	go http.Serve(l, relay)
	url := "ws://" + l.Addr().String() + "/gun"
	phone := gundb.New(gundb.Options{Peers: []string{url}})  // ali's
	laptop := gundb.New(gundb.Options{Peers: []string{url}}) // ali's
	bobs := gundb.New(gundb.Options{Peers: []string{url}})   // bob's
	defer phone.Close()
	defer laptop.Close()
	defer bobs.Close()

	// Sign up on the phone. Writes through ali's refs are signed by her key.
	ali, err := phone.CreateUser(ctx, "ali", "correct horse battery")
	check(err)
	check(ali.Get("profile").PutAck(ctx, Profile{Name: "Ali"}))

	// Anyone can read her space, by public key.
	p, err := laptop.User(ali.Pub()).Get("profile").Once[Profile](ctx)
	check(err)
	fmt.Println("profile:", p.Name) // profile: Ali

	// Nobody else can write it: unsigned writes are rejected everywhere.
	err = laptop.User(ali.Pub()).Get("profile").Get("name").Put(ctx, "Mallory")
	fmt.Println("forged write:", errors.Is(err, gundb.ErrUnverified)) // forged write: true

	// Log in on the laptop with the password: same keys, same space.
	ali, err = laptop.Login(ctx, "ali", "correct horse battery")
	check(err)
	check(ali.Get("profile").Get("name").PutAck(ctx, "Ali N."))

	// Private messages: encrypt with a secret only ali and bob can derive.
	bob, err := bobs.CreateUser(ctx, "bob", "staple battery horse")
	check(err)
	bobEPub, err := laptop.User(bob.Pub()).Get("epub").Once[string](ctx)
	check(err)
	secret, err := sea.Secret(bobEPub, ali.Pair())
	check(err)
	enc, err := sea.Encrypt("meet at 6", secret)
	check(err)
	check(ali.Get("to-bob").PutAck(ctx, enc)) // public, but unreadable

	// Bob, on his device, derives the same secret from ali's public epub.
	stored, err := bobs.User(ali.Pub()).Get("to-bob").Once[string](ctx)
	check(err)
	aliEPub, err := bobs.User(ali.Pub()).Get("epub").Once[string](ctx)
	check(err)
	shared, err := sea.Secret(aliEPub, bob.Pair())
	check(err)
	plain, err := sea.Decrypt(stored, shared)
	check(err)
	var msg string
	check(json.Unmarshal(plain, &msg))
	fmt.Println("bob reads:", msg) // bob reads: meet at 6
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
