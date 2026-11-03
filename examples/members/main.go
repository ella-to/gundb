// A private list: members holds one entry per user. Each user may read
// their own entry, members.<name>, and the node it links to,
// members/<name>, but listing members shows them nobody else. An admin
// sees everyone.
//
// CanRead is checked per field, so the list node itself can be partly
// readable: a get of members returns only the fields the reader may see.
package main

import (
	"context"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"

	"ella.to/gundb"
)

type Member struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// session is what the middleware stores in each connection's context.
type session struct {
	user  string
	admin bool
}

type sessionKey struct{}

// sessionOf returns the session behind a connection's context.
func sessionOf(ctx context.Context) session {
	s, _ := ctx.Value(sessionKey{}).(session)
	return s
}

// tokens stands in for your real login (see examples/login).
var tokens = map[string]session{
	"tok-ali":  {user: "ali"},
	"tok-bob":  {user: "bob"},
	"tok-root": {user: "root", admin: true},
}

func authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := tokens[r.URL.Query().Get("token")]
		if !ok {
			http.Error(w, "not logged in", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, s)))
	})
}

// entryOwner returns whose entry soul.field is part of: members.ali and
// members/ali.<anything> belong to ali. ok is false outside members.
func entryOwner(soul, field string) (owner string, ok bool) {
	if soul == "members" {
		return field, true
	}
	rest, ok := strings.CutPrefix(soul, "members/")
	owner, _, _ = strings.Cut(rest, "/")
	return owner, ok
}

func main() {
	ctx := context.Background()

	mine := func(ctx context.Context, soul, field string) bool {
		owner, ok := entryOwner(soul, field)
		s := sessionOf(ctx)
		return !ok || s.admin || owner == s.user
	}
	relay := gundb.New(gundb.Options{CanRead: mine, CanWrite: mine})
	defer relay.Close()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	go http.Serve(l, authenticate(relay))
	url := "ws://" + l.Addr().String() + "/gun?token="
	connect := func(token string) *gundb.DB {
		db := gundb.New(gundb.Options{Peers: []string{url + token}})
		check(db.Get("ping").PutAck(ctx, map[string]any{"ok": true})) // wait until connected
		return db
	}

	// Everyone fills in their own entry. Keyed Puts give each entry the
	// soul members/<name>, which the rules can match.
	ali, bob := connect("tok-ali"), connect("tok-bob")
	defer ali.Close()
	defer bob.Close()
	check(ali.Get("members").Get("ali").PutAck(ctx, Member{Name: "Ali", Email: "ali@example.com"}))
	check(bob.Get("members").Get("bob").PutAck(ctx, Member{Name: "Bob", Email: "bob@example.com"}))

	// A fresh connection for Ali, so nothing is cached locally.
	ali2 := connect("tok-ali")
	defer ali2.Close()

	// Ali reads her own entry, by path or by soul.
	me, err := ali2.Get("members").Get("ali").Once[Member](ctx)
	check(err)
	fmt.Println("ali reads her entry:", me.Email) // ali@example.com

	// Bob's entry does not exist as far as Ali can tell.
	_, err = ali2.Get("members").Get("bob").Once[Member](ctx)
	fmt.Println("ali reads bob's entry:", err) // gundb: not found
	_, err = ali2.Get("members/bob").Once[Member](ctx)
	fmt.Println("ali reads bob's node:", err) // gundb: not found

	// Listing the members shows only her.
	list, err := ali2.Get("members").Once[map[string]any](ctx)
	check(err)
	fmt.Println("ali lists members:", slices.Sorted(maps.Keys(list))) // [ali]

	// The admin sees everyone.
	root := connect("tok-root")
	defer root.Close()
	all, err := root.Get("members").Once[map[string]any](ctx)
	check(err)
	fmt.Println("root lists members:", slices.Sorted(maps.Keys(all))) // [ali bob]
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
