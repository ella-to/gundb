// A chat where everyone logs in with a user name and password. The server
// hands out a token, the client connects with it, and the relay lets each
// user post only under their own name.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"ella.to/gundb"
)

type Message struct {
	Text string `json:"text"`
}

// passwords stands in for your user database. Store hashes (bcrypt,
// argon2), never plain passwords.
var passwords = map[string]string{"ali": "ali-secret", "bob": "bob-secret"}

// sessions maps a token to the user it was issued to.
var (
	mu       sync.Mutex
	sessions = map[string]string{}
)

// login checks a user name and password and returns a new token.
//
//	POST /login  user=ali&password=ali-secret  ->  200 <token>
func login(w http.ResponseWriter, r *http.Request) {
	user, password := r.FormValue("user"), r.FormValue("password")
	want, ok := passwords[user]
	if !ok || subtle.ConstantTimeCompare([]byte(password), []byte(want)) != 1 {
		http.Error(w, "wrong user or password", http.StatusUnauthorized)
		return
	}
	token := rand.Text()
	mu.Lock()
	sessions[token] = user
	mu.Unlock()
	io.WriteString(w, token)
}

func main() {
	ctx := context.Background()

	relay := gundb.New(gundb.Options{
		// Authentication: who is connecting? The token comes in the URL
		// (browsers cannot set WebSocket headers). No valid token, no
		// connection: only logged-in users can read or post.
		Authenticate: func(r *http.Request) (string, error) {
			mu.Lock()
			user, ok := sessions[r.URL.Query().Get("token")]
			mu.Unlock()
			if !ok {
				return "", errors.New("not logged in")
			}
			return user, nil
		},
		// Authorization: what may they do? Messages live at
		// chat/<user>/<id>, and only <user> may write there.
		CanWrite: func(user, soul, field string) bool {
			path := strings.Split(soul+"/"+field, "/") // chat, ali, m1, text
			return path[0] == "chat" && path[1] == user
		},
	})
	defer relay.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", login)
	mux.Handle("/gun", relay)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	go http.Serve(l, mux)
	server := "http://" + l.Addr().String()

	// A wrong password gets no token.
	_, err = connect(server, "bob", "guess")
	fmt.Println("bob, wrong password:", err) // 401 Unauthorized

	ali, err := connect(server, "ali", "ali-secret")
	check(err)
	defer ali.Close()
	bob, err := connect(server, "bob", "bob-secret")
	check(err)
	defer bob.Close()

	// Ali posts under her own name...
	check(ali.Get("chat").Get("ali").Get("m1").PutAck(ctx, Message{Text: "hi, I'm Ali"}))

	// ...and Bob, logged in too, reads it.
	m, err := bob.Get("chat").Get("ali").Get("m1").Once[Message](ctx)
	check(err)
	fmt.Println("bob reads:", m.Text) // bob reads: hi, I'm Ali

	// Bob cannot post as Ali.
	err = bob.Get("chat").Get("ali").Get("m2").PutAck(ctx, Message{Text: "I'm Ali, honest"})
	fmt.Println("bob posts as ali:", errors.Is(err, gundb.ErrForbidden)) // true
}

// connect logs in and returns a DB connected to the relay with the token.
func connect(server, user, password string) (*gundb.DB, error) {
	resp, err := http.PostForm(server+"/login", url.Values{"user": {user}, "password": {password}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}
	token, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	ws := "ws" + strings.TrimPrefix(server, "http") + "/gun?token=" + url.QueryEscape(string(token))
	return gundb.New(gundb.Options{Peers: []string{ws}}), nil
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
