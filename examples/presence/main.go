// Who is online, on how many devices and tabs. The client sends a device
// ID with its connection; Authenticate returns it as the custom ID, and
// OnConnect / OnDisconnect count connections per user and per device.
//
// In a browser, keep the device ID in localStorage (shared by every tab of
// that browser) and connect to /gun?token=...&device=<id>.
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

// presence counts open connections per user and device.
type presence struct {
	mu      sync.Mutex
	devices map[string]map[string]int // user -> device -> open tabs
	events  chan string               // only so this example prints in order
}

func (p *presence) connect(c gundb.PeerInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.devices[c.User] == nil {
		p.devices[c.User] = map[string]int{}
		p.events <- c.User + " is online"
	}
	p.devices[c.User][c.CustomID]++
	p.events <- fmt.Sprintf("%s: %d tab(s) on %s", c.User, p.devices[c.User][c.CustomID], c.CustomID)
}

func (p *presence) disconnect(c gundb.PeerInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	tabs := p.devices[c.User]
	if tabs[c.CustomID]--; tabs[c.CustomID] == 0 {
		delete(tabs, c.CustomID)
	}
	p.events <- fmt.Sprintf("%s: %d tab(s) on %s", c.User, tabs[c.CustomID], c.CustomID)
	if len(tabs) == 0 {
		delete(p.devices, c.User)
		p.events <- c.User + " is offline"
	}
}

func main() {
	ctx := context.Background()
	online := &presence{devices: map[string]map[string]int{}, events: make(chan string, 16)}

	relay := gundb.New(gundb.Options{
		Authenticate: func(r *http.Request) (string, string, error) {
			// A real app looks the token up (see examples/login).
			user, ok := strings.CutPrefix(r.URL.Query().Get("token"), "tok-")
			if !ok {
				return "", "", errors.New("not logged in")
			}
			// The device ID comes from the client, so it labels connections;
			// it proves nothing. Trust only the user for permissions.
			return user, r.URL.Query().Get("device"), nil
		},
		OnConnect:    online.connect,
		OnDisconnect: online.disconnect,
	})
	defer relay.Close()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	go http.Serve(l, relay)
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
