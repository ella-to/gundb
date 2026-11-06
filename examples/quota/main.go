// Plans and quotas on a relay. Each user keeps their data under
// users/<name>/..., and their plan says how much: free is 1,000 nodes and
// 10 MB, pro is 10,000 nodes and 100 MB.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"ella.to/gundb"
)

type Note struct {
	Text string `json:"text"`
}

var plans = map[string]gundb.Limit{
	"free": {Nodes: 1_000, Bytes: 10 << 20},
	"pro":  {Nodes: 10_000, Bytes: 100 << 20},
}

// accounts knows each user's plan. It stands in for your billing
// database: load it at start and update it when someone pays, so the
// quota never waits for I/O.
type accounts struct {
	mu   sync.RWMutex
	plan map[string]string
}

func (a *accounts) Limit(user string) gundb.Limit {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if p, ok := a.plan[user]; ok {
		return plans[p]
	}
	return plans["free"]
}

func (a *accounts) SetPlan(user, plan string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.plan[user] = plan
}

// owner returns whose data a node is: "users/ali/notes/1" -> "ali".
func owner(soul string) string {
	rest, ok := strings.CutPrefix(soul, "users/")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, "/")
	return name
}

// The login of section 8, in short: the token in the URL names the user.
type userKey struct{}

func authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := strings.CutPrefix(r.URL.Query().Get("token"), "tok-")
		if !ok {
			http.Error(w, "unknown token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, user)))
	})
}

func main() {
	ctx := context.Background()
	accts := &accounts{plan: map[string]string{"ali": "free"}}

	relay := gundb.New(gundb.Options{
		// Everyone writes only their own users/<name>/... CanWrite checks
		// soul + "/" + field, so the link users.ali is ali's to write too.
		CanWrite: func(ctx context.Context, soul, field string) bool {
			user, _ := ctx.Value(userKey{}).(string)
			return owner(soul+"/"+field) == user
		},
		// Every node under users/<name> counts against <name>'s plan.
		Quota: &gundb.Quota{Owner: owner, Limit: accts.Limit},
	})
	defer relay.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	go http.Serve(ln, authenticate(relay))

	ali := gundb.New(gundb.Options{Peers: []string{"ws://" + ln.Addr().String() + "/gun?token=tok-ali"}})
	defer ali.Close()
	notes := ali.Get("users").Get("ali").Get("notes")

	// Write notes until the free plan is full.
	n := 0
	for ; ; n++ {
		err := notes.Get(fmt.Sprint(n)).PutAck(ctx, Note{Text: "note"})
		if errors.Is(err, gundb.ErrQuota) {
			fmt.Printf("note %d refused: %v\n", n, err)
			break
		}
		if err != nil {
			log.Fatal(err)
		}
	}
	u := relay.Usage("ali")
	fmt.Printf("ali keeps %d nodes, %d bytes\n", u.Nodes, u.Bytes)

	// Deleting a note frees its node: null its fields, and the link to it.
	notes.Get("0").Get("text").PutAck(ctx, nil)
	notes.Get("0").PutAck(ctx, nil)
	fmt.Printf("after deleting note 0: %d nodes\n", relay.Usage("ali").Nodes)
	if err := notes.Get(fmt.Sprint(n)).PutAck(ctx, Note{Text: "fits now"}); err != nil {
		log.Fatal(err)
	}

	// An upgrade takes effect on the next write.
	accts.SetPlan("ali", "pro")
	if err := notes.Get(fmt.Sprint(n+1)).PutAck(ctx, Note{Text: "pro"}); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("on pro: %d nodes, limit %d\n", relay.Usage("ali").Nodes, accts.Limit("ali").Nodes)

	// For billing: everyone's usage.
	for user, u := range relay.Usages() {
		fmt.Printf("bill %s: %d nodes, %.1f KB\n", user, u.Nodes, float64(u.Bytes)/1024)
	}
}
