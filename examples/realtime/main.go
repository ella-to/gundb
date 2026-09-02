// Subscribe with On: the callback runs now and on every change.
package main

import (
	"context"
	"fmt"
	"time"

	"ella.to/gundb"
)

func main() {
	ctx := context.Background()
	db := gundb.New()
	defer db.Close()

	topic := db.Get("room").Get("topic")

	off := topic.On(func(t string) {
		fmt.Println("topic is now:", t)
	})
	defer off()

	topic.Put(ctx, "Hello")
	time.Sleep(50 * time.Millisecond)
	topic.Put(ctx, "Go + GUN")
	time.Sleep(50 * time.Millisecond)
}
