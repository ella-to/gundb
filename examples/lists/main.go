// Lists: Set adds items, Map().On watches them, Put(nil) removes one.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"ella.to/gundb"
)

type Todo struct {
	Title string `json:"title"`
	Done  bool   `json:"done"`
}

func main() {
	ctx := context.Background()
	db := gundb.New()
	defer db.Close()

	todos := db.Get("todos")

	// A pointer type gets nil when an item is removed.
	off := todos.Map().On(func(id string, t *Todo) {
		if t == nil {
			fmt.Println("removed", id)
			return
		}
		fmt.Printf("%-10s done=%v\n", t.Title, t.Done)
	})
	defer off()

	milk, err := todos.Set(ctx, Todo{Title: "Buy milk"})
	check(err)
	_, err = todos.Set(ctx, Todo{Title: "Learn Go"})
	check(err)
	time.Sleep(50 * time.Millisecond)

	check(milk.Get("done").Put(ctx, true)) // update an item
	time.Sleep(50 * time.Millisecond)

	check(todos.Get(milk.Key()).Put(ctx, nil)) // remove it from the list
	time.Sleep(50 * time.Millisecond)

	all, err := todos.Once[map[string]Todo](ctx)
	check(err)
	fmt.Println(len(all), "item left")
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
