// Who is online, on how many devices and tabs. The client sends a device
// ID with its connection; HTTP middleware stores it in the request context
// next to the user, and OnConnect / OnDisconnect, which get that context,
// count connections per user and per device.
//
// In a browser, keep the device ID in localStorage (shared by every tab of
// that browser) and connect to /gun?token=...&device=<id>.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"ella.to/gundb"
)

// presence counts open connections per user and device.
type presence struct {
	mu      sync.Mutex
	devices map[string]map[string]int // user -> device -> open tabs
	events  chan string               // only so this example prints in order
}

// session is what the middleware stores in each connection's context.
type session struct{ user, device string }

type sessionKey struct{}

// authenticate is HTTP middleware in front of the relay.
func authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A real app looks the token up (see examples/login).
		user, ok := strings.CutPrefix(r.URL.Query().Get("token"), "tok-")
		if !ok {
			http.Error(w, "not logged in", http.StatusUnauthorized)
			return
		}
		// The device ID comes from the client, so it labels connections;
		// it proves nothing. Trust only the user for permissions.
		s := &session{user: user, device: r.URL.Query().Get("device")}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, s)))
	})
}

// connect and disconnect get the connection's context. By OnDisconnect it
// is done, but the session is still in it.
func (p *presence) connect(ctx context.Context, _ gundb.PeerInfo) {
	c := ctx.Value(sessionKey{}).(*session)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.devices[c.user] == nil {
		p.devices[c.user] = map[string]int{}
		p.events <- c.user + " is online"
	}
	p.devices[c.user][c.device]++
	p.events <- fmt.Sprintf("%s: %d tab(s) on %s", c.user, p.devices[c.user][c.device], c.device)
}

func (p *presence) disconnect(ctx context.Context, _ gundb.PeerInfo) {
	c := ctx.Value(sessionKey{}).(*session)
	p.mu.Lock()
	defer p.mu.Unlock()
	tabs := p.devices[c.user]
	if tabs[c.device]--; tabs[c.device] == 0 {
		delete(tabs, c.device)
	}
	p.events <- fmt.Sprintf("%s: %d tab(s) on %s", c.user, tabs[c.device], c.device)
	if len(tabs) == 0 {
		delete(p.devices, c.user)
		p.events <- c.user + " is offline"
	}
}

func main() {
	ctx := context.Background()
	online := &presence{devices: map[string]map[string]int{}, events: make(chan string, 16)}

	relay := gundb.New(gundb.Options{
		OnConnect:    online.connect,
		OnDisconnect: online.disconnect,
	})
	defer relay.Close()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	go http.Serve(l, authenticate(relay))
	url := "ws://" + l.Addr().String() + "/gun?token=tok-ali&device="

	// Each tab is its own DB and its own connection.
	open := func(device string) *gundb.DB {
		db := gundb.New(gundb.Options{Peers: []string{url + device}})
		check(db.Get("ping").PutAck(ctx, map[string]any{"ok": true})) // wait until connected
		return db
	}
	tab1 := open("laptop")
	fmt.Println(<-online.events) // ali is online
	fmt.Println(<-online.events) // ali: 1 tab(s) on laptop
	tab2 := open("laptop")
	fmt.Println(<-online.events) // ali: 2 tab(s) on laptop
	phone := open("phone")
	fmt.Println(<-online.events) // ali: 1 tab(s) on phone

	tab1.Close()
	fmt.Println(<-online.events) // ali: 1 tab(s) on laptop
	tab2.Close()
	fmt.Println(<-online.events) // ali: 0 tab(s) on laptop
	phone.Close()
	fmt.Println(<-online.events) // ali: 0 tab(s) on phone
	fmt.Println(<-online.events) // ali is offline
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
