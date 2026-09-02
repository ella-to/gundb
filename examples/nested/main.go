// Nested structs become linked nodes, and you can follow links by path.
package main

import (
	"context"
	"fmt"
	"log"

	"ella.to/gundb"
)

type Pet struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type Person struct {
	Name string  `json:"name"`
	Pet  *Pet    `json:"pet,omitempty"`
	Boss *Person `json:"boss,omitempty"`
}

func main() {
	ctx := context.Background()
	db := gundb.New()
	defer db.Close()

	// The pet is stored as its own node ("mark/pet"), linked from mark.
	check(db.Get("mark").Put(ctx, Person{Name: "Mark", Pet: &Pet{Name: "Fluffy", Kind: "cat"}}))

	pet, err := db.Get("mark").Get("pet").Once[Pet](ctx)
	check(err)
	fmt.Println(pet.Name, "is a", pet.Kind) // Fluffy is a cat

	// Link two existing nodes by putting a ref.
	check(db.Get("amy").Put(ctx, Person{Name: "Amy"}))
	check(db.Get("mark").Get("boss").Put(ctx, db.Get("amy")))

	// Once loads linked nodes into nested structs.
	mark, err := db.Get("mark").Once[Person](ctx)
	check(err)
	fmt.Println(mark.Name, "works for", mark.Boss.Name) // Mark works for Amy

	// Writing through a link changes the linked node itself.
	check(db.Get("mark").Get("boss").Get("name").Put(ctx, "Amy B."))
	amy, err := db.Get("amy").Once[Person](ctx)
	check(err)
	fmt.Println(amy.Name) // Amy B.
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
