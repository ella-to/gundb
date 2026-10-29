// A list of links: each user has a set of friends that point at other
// user nodes. Add, update, remove and watch them.
package main

import (
	"context"
	"fmt"
	"log"
	"maps"
	"slices"
	"time"

	"ella.to/gundb"
)

type User struct {
	Name string `json:"name"`
	City string `json:"city"`
}

func main() {
	ctx := context.Background()
	db := gundb.New()
	defer db.Close()

	// Every user is a node under its own key.
	for key, u := range map[string]User{
		"alice": {Name: "Alice", City: "Paris"},
		"bob":   {Name: "Bob", City: "Berlin"},
		"carol": {Name: "Carol", City: "Rome"},
	} {
		check(db.Get(key).Put(ctx, u))
	}
	friends := db.Get("alice").Get("friends")

	// Watch the list. The key is the friend's soul; a removed friend
	// arrives as nil.
	off := friends.Map().On(func(key string, u *User) {
		if u == nil {
			fmt.Println("removed", key)
			return
		}
		fmt.Printf("friend %s: %s from %s\n", key, u.Name, u.City)
	})
	defer off()

	// Add: Set a ref to an existing user. This stores a link, not a copy.
	_, err := friends.Set(ctx, db.Get("bob"))
	check(err)
	_, err = friends.Set(ctx, db.Get("carol"))
	check(err)
	time.Sleep(50 * time.Millisecond)

	// Friendship both ways is just another link back.
	_, err = db.Get("bob").Get("friends").Set(ctx, db.Get("alice"))
	check(err)

	// Update: change the user node. Every list that links to it sees it.
	check(db.Get("bob").Get("city").Put(ctx, "Madrid"))
	time.Sleep(50 * time.Millisecond)

	// Remove: delete the link by its key. Bob's node itself is untouched.
	check(friends.Get("bob").Put(ctx, nil))
	time.Sleep(50 * time.Millisecond)

	// Read the whole list once. Removed friends are left out.
	all, err := friends.Once[map[string]User](ctx)
	check(err)
	fmt.Println(slices.Sorted(maps.Keys(all))) // [carol]

	bob, err := db.Get("bob").Once[User](ctx)
	check(err)
	fmt.Println(bob.Name, "still exists, in", bob.City) // Bob still exists, in Madrid

	// Add the same friend again: the link comes back.
	_, err = friends.Set(ctx, db.Get("bob"))
	check(err)
	time.Sleep(50 * time.Millisecond)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
