// Circular links: two users who are each other's friend, and choosing
// your own key for a node.
package main

import (
	"context"
	"fmt"
	"log"

	"ella.to/gundb"
)

type User struct {
	Name   string `json:"name"`
	Friend *User  `json:"friend,omitempty"`
}

func main() {
	ctx := context.Background()
	db := gundb.New()
	defer db.Close()

	// A Go pointer cycle can't be Put: encoding/json refuses it.
	alice := &User{Name: "Alice"}
	alice.Friend = &User{Name: "Bob", Friend: alice}
	fmt.Println(db.Get("alice").Put(ctx, alice) != nil) // true

	// Instead, Put each user under its own key, then link them with refs.
	// The key you pass to db.Get is the node's soul.
	check(db.Get("alice").Put(ctx, User{Name: "Alice"}))
	check(db.Get("bob").Put(ctx, User{Name: "Bob"}))
	check(db.Get("alice").Get("friend").Put(ctx, db.Get("bob")))
	check(db.Get("bob").Get("friend").Put(ctx, db.Get("alice")))

	// Paths follow links as far as you like.
	name, err := db.Get("alice").Get("friend").Get("friend").Get("friend").Get("name").Once[string](ctx)
	check(err)
	fmt.Println(name) // Bob

	// Once stops where the cycle closes: the link back to alice comes back
	// as {"#": "alice"}, which decodes into an empty User.
	a, err := db.Get("alice").Once[User](ctx)
	check(err)
	fmt.Printf("%s -> %s -> %q\n", a.Name, a.Friend.Name, a.Friend.Friend.Name) // Alice -> Bob -> ""

	raw, err := db.Get("alice").Once[map[string]any](ctx)
	check(err)
	fmt.Println(raw["friend"].(map[string]any)["friend"]) // map[#:alice]

	// Writing through a link changes the linked node, from either side.
	check(db.Get("bob").Get("friend").Get("name").Put(ctx, "Alice A."))
	name, err = db.Get("alice").Get("name").Once[string](ctx)
	check(err)
	fmt.Println(name) // Alice A.

	// Your own key for a list item: Put the node under the key you want,
	// then add a ref to it with Set. The item's key in the list is its soul.
	carol, err := db.Get("people").Set(ctx, db.Get("my-own-key"))
	check(err)
	check(db.Get("my-own-key").Put(ctx, User{Name: "Carol"}))
	fmt.Println(carol.Key()) // my-own-key
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
