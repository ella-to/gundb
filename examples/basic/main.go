// Put and read data. No server needed: a DB on its own is a local database.
package main

import (
	"context"
	"fmt"
	"log"

	"ella.to/gundb"
)

type User struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func main() {
	ctx := context.Background()
	db := gundb.New()
	defer db.Close()

	alice := db.Get("users").Get("alice")
	check(alice.Put(ctx, User{Name: "Alice", Age: 30}))
	check(alice.Get("age").Put(ctx, 31)) // update a single field

	user, err := alice.Once[User](ctx)
	check(err)
	fmt.Println(user.Name, user.Age) // Alice 31

	name, err := alice.Get("name").Once[string](ctx)
	check(err)
	fmt.Println(name) // Alice

	_, err = db.Get("users").Get("bob").Once[User](ctx)
	fmt.Println(err) // gundb: not found
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
