// Read and write permissions on a relay. Everyone can read
// users/<name>/messages, only <name> can write it, and users/<name>/private
// is for <name>'s eyes only.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"

	"ella.to/gundb"
)

type Message struct {
	Text string `json:"text"`
}

// tokens stands in for your real login: sessions, JWTs, API keys...
var tokens = map[string]string{"tok-ali": "ali", "tok-bob": "bob"}

// owner returns who a path belongs to: "users/ali/messages/m1" -> "ali".
func owner(path string) string {
	rest, ok := strings.CutPrefix(path, "users/")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, "/")
	return name
}

func main() {
	ctx := context.Background()

	relay := gundb.New(gundb.Options{
		// Who is connecting? Browsers cannot set WebSocket headers, so the
		// token comes in the URL. No token: an anonymous, read-only user.
		Authenticate: func(r *http.Request) (string, error) {
			token := r.URL.Query().Get("token")
			if token == "" {
				return "", nil
			}
			if user, ok := tokens[token]; ok {
				return user, nil
			}
			return "", errors.New("unknown token")
		},
		// Write: users/<name>/... belongs to <name>. The check is on
		// soul + "/" + field, so the link users -> ali is protected too.
		CanWrite: func(user, soul, field string) bool {
			if o := owner(soul + "/" + field); o != "" {
				return o == user
			}
			return user != "" // anything else: any logged-in user
		},
		// Read: everything, except users/<name>/private.
		CanRead: func(user, soul string) bool {
			if o := owner(soul); o != "" && strings.HasPrefix(soul, "users/"+o+"/private") {
				return o == user
			}
			return true
		},
	})
	defer relay.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	go http.Serve(l, relay)
	url := "ws://" + l.Addr().String() + "/gun"

	ali := gundb.New(gundb.Options{Peers: []string{url + "?token=tok-ali"}})
	bob := gundb.New(gundb.Options{Peers: []string{url + "?token=tok-bob"}})
	defer ali.Close()
	defer bob.Close()

	// Ali writes her own messages. Use keyed Puts, not Set, for protected
	// data: m1 gets the soul users/ali/messages/m1, which the rules can
	// match; Set would give it a random soul.
	messages := ali.Get("users").Get("ali").Get("messages")
	check(messages.Get("m1").PutAck(ctx, Message{Text: "Hello from Ali"}))
	check(ali.Get("users").Get("ali").Get("private").Get("note").PutAck(ctx, "buy a gift for Bob"))

	// Bob can read them...
	m, err := bob.Get("users").Get("ali").Get("messages").Get("m1").Once[Message](ctx)
	check(err)
	fmt.Println("bob reads:", m.Text) // bob reads: Hello from Ali

	// ...but not write them. PutAck reports the relay's answer.
	err = bob.Get("users").Get("ali").Get("messages").Get("m1").PutAck(ctx, Message{Text: "hacked"})
	fmt.Println("bob writes:", errors.Is(err, gundb.ErrForbidden)) // bob writes: true

	// And Ali's private node is never sent to him.
	_, err = bob.Get("users").Get("ali").Get("private").Get("note").Once[string](ctx)
	fmt.Println("bob reads the private note:", err) // gundb: not found
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
