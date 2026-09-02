// A chat room shared by browsers (GUN.js) and a Go bot, through a Go relay.
//
//	go run ./examples/chat
//
// then open http://localhost:8765 in two tabs.
package main

import (
	"context"
	_ "embed"
	"log"
	"net/http"
	"strings"
	"time"

	"ella.to/gundb"
)

//go:embed index.html
var page []byte

type Message struct {
	Who  string `json:"who"`
	Text string `json:"text"`
	At   int64  `json:"at"`
}

func main() {
	db := gundb.New()
	defer db.Close()

	// The bot answers every new message that isn't its own.
	start := time.Now().UnixMilli()
	chat := db.Get("chat")
	chat.Map().On(func(_ string, m Message) {
		if m.Who == "bot" || m.At < start {
			return
		}
		reply := Message{Who: "bot", Text: strings.ToUpper(m.Text) + "!", At: time.Now().UnixMilli()}
		if _, err := chat.Set(context.Background(), reply); err != nil {
			log.Print(err)
		}
	})

	http.Handle("/gun", db)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.Write(page) })
	log.Print("chat on http://localhost:8765")
	log.Fatal(http.ListenAndServe(":8765", nil))
}
