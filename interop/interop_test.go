// Package interop checks wire compatibility against the real JavaScript GUN.
//
// The tests skip unless Node.js is installed and the JS deps are present:
//
//	cd interop/testdata && npm install
//	go test ./interop
package interop_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ella.to/gundb"
)

type Pet struct {
	Name string `json:"name"`
}

type Person struct {
	Name string `json:"name"`
	Pet  *Pet   `json:"pet,omitempty"`
}

type Item struct {
	Title string `json:"title"`
}

// Go client talking to a stock JS relay (node, AXE, radisk).
func TestGoClientWithJSServer(t *testing.T) {
	requireNode(t)
	ctx := t.Context()
	peer := startJSServer(t)
	db := gundb.New(gundb.Options{Peers: []string{peer}})
	defer db.Close()

	t.Run("JS reads what Go wrote", func(t *testing.T) {
		err := db.Get("mark").PutAck(ctx, Person{Name: "Mark", Pet: &Pet{Name: "Fluffy"}})
		if err != nil {
			t.Fatal(err)
		}
		assertJSON(t, runJS(t, peer, "once", "mark"), `{"name":"Mark","pet":{"#":"mark/pet"}}`)
		assertJSON(t, runJS(t, peer, "once", "mark", "pet"), `{"name":"Fluffy"}`)
		assertJSON(t, runJS(t, peer, "once", "mark", "pet", "name"), `"Fluffy"`)
	})

	t.Run("Go reads what JS wrote", func(t *testing.T) {
		assertJSON(t, runJS(t, peer, "put", "amy", `{"name":"Amy","pet":{"name":"Rex"}}`), `{"ok":true}`)
		p, err := db.Get("amy").Once[Person](ctx)
		if err != nil || p.Name != "Amy" || p.Pet == nil || p.Pet.Name != "Rex" {
			t.Fatalf("%+v %v", p, err)
		}
	})

	t.Run("Go sees live JS writes", func(t *testing.T) {
		got := make(chan string, 4)
		defer db.Get("room").Get("topic").On(func(s string) { got <- s })()
		time.Sleep(300 * time.Millisecond) // subscription reaches the relay
		runJS(t, peer, "put", "room", `{"topic":"hello from js"}`)
		waitFor(t, got, "hello from js")
	})

	t.Run("JS sees live Go writes", func(t *testing.T) {
		wait := startJS(t, peer, "watch", "room2", "topic", "hello from go")
		time.Sleep(300 * time.Millisecond)
		db.Get("room2").Get("topic").Put(ctx, "hello from go")
		assertJSON(t, wait(), `"hello from go"`)
	})

	t.Run("sets work both ways", func(t *testing.T) {
		db.Get("list").Set(ctx, Item{Title: "go 1"})
		db.Get("list").Set(ctx, Item{Title: "go 2"})
		assertJSON(t, runJS(t, peer, "map", "list", "2"), `["go 1","go 2"]`)

		runJS(t, peer, "set", "list", `{"title":"js 1"}`)
		titles := make(chan string, 8)
		defer db.Get("list").Map().On(func(_ string, it Item) { titles <- it.Title })()
		waitFor(t, titles, "js 1")
	})
}

// Go SEA users through a stock JS relay, which verifies every signed
// write itself and would refuse to store Go signatures it rejects.
func TestGoUsersThroughJSRelay(t *testing.T) {
	requireNode(t)
	ctx := t.Context()
	peer := startJSServer(t)
	a := gundb.New(gundb.Options{Peers: []string{peer}})
	defer a.Close()
	u, err := a.CreateUser(ctx, "relayuser", "relay password")
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Get("profile").PutAck(ctx, Person{Name: "Signed in Go"}); err != nil {
		t.Fatalf("JS relay refused the Go-signed write: %v", err)
	}

	b := gundb.New(gundb.Options{Peers: []string{peer}}) // knows nothing yet
	defer b.Close()
	again, err := b.Login(ctx, "relayuser", "relay password")
	if err != nil || again.Pub() != u.Pub() {
		t.Fatalf("login through the JS relay: %v", err)
	}
	name, err := b.User(u.Pub()).Get("profile").Get("name").Once[string](ctx)
	if err != nil || name != "Signed in Go" {
		t.Fatalf("read through the JS relay: %q, %v", name, err)
	}
	assertJSON(t, runScript(t, "user-client.js", peer, "read", u.Pub(), "profile", "name"), `"Signed in Go"`)
}

// JS browser clients using a Go relay.
func TestJSClientsWithGoRelay(t *testing.T) {
	requireNode(t)
	ctx := t.Context()
	relay := gundb.New()
	defer relay.Close()
	srv := httptest.NewServer(relay)
	defer srv.Close()
	peer := srv.URL + "/gun"

	t.Run("JS to JS through Go", func(t *testing.T) {
		wait := startJS(t, peer, "watch", "chat", "msg", "yo")
		time.Sleep(300 * time.Millisecond)
		assertJSON(t, runJS(t, peer, "put", "chat", `{"msg":"yo"}`), `{"ok":true}`)
		assertJSON(t, wait(), `"yo"`)

		msg, err := relay.Get("chat").Get("msg").Once[string](ctx)
		if err != nil || msg != "yo" {
			t.Fatalf("relay has %q %v", msg, err)
		}
	})

	t.Run("JS reads nested data from Go", func(t *testing.T) {
		relay.Get("mark").Put(ctx, Person{Name: "Mark", Pet: &Pet{Name: "Fluffy"}})
		assertJSON(t, runJS(t, peer, "once", "mark", "pet", "name"), `"Fluffy"`)
	})

	t.Run("JS set and map through Go", func(t *testing.T) {
		wait := startJS(t, peer, "map", "todo", "2")
		time.Sleep(300 * time.Millisecond)
		runJS(t, peer, "set", "todo", `{"title":"a"}`)
		runJS(t, peer, "set", "todo", `{"title":"b"}`)
		assertJSON(t, wait(), `["a","b"]`)
	})
}

// GUN.js browser clients logging in to a Go relay with write rules. The
// token rides in the peer URL, as browsers cannot set WebSocket headers.
func TestJSClientsWithAuthRelay(t *testing.T) {
	requireNode(t)
	relay := gundb.New(gundb.Options{
		Authenticate: func(r *http.Request) (string, error) {
			return strings.TrimPrefix(r.URL.Query().Get("token"), "tok-"), nil
		},
		CanWrite: func(user, soul, field string) bool {
			owner, _, _ := strings.Cut(strings.TrimPrefix(soul, "users/"), "/")
			return !strings.HasPrefix(soul, "users/") || owner == user
		},
	})
	defer relay.Close()
	srv := httptest.NewServer(relay)
	defer srv.Close()
	peer := srv.URL + "/gun"

	assertJSON(t, runJS(t, peer+"?token=tok-ali", "put", "users/ali", `{"text":"hi"}`), `{"ok":true}`)

	var res struct{ Err string }
	out := runJS(t, peer+"?token=tok-bob", "put", "users/ali", `{"text":"hacked"}`)
	if json.Unmarshal([]byte(out), &res) != nil || !strings.Contains(res.Err, "forbidden") {
		t.Fatalf("bob's write: got %s, want a forbidden error", out)
	}
	assertJSON(t, runJS(t, peer+"?token=tok-bob", "once", "users/ali", "text"), `"hi"`)
}

// SEA users between GUN.js and Go, through a Go relay that verifies every
// signed write.
func TestSEAUsersWithJS(t *testing.T) {
	requireNode(t)
	ctx := t.Context()
	relay := gundb.New()
	defer relay.Close()
	srv := httptest.NewServer(relay)
	defer srv.Close()
	peer := srv.URL + "/gun"
	goClient := func() *gundb.DB {
		db := gundb.New(gundb.Options{Peers: []string{peer}})
		t.Cleanup(func() { db.Close() })
		return db
	}
	var res struct{ Pub, Err string }
	parse := func(out string) {
		t.Helper()
		res.Pub, res.Err = "", ""
		if json.Unmarshal([]byte(out), &res) != nil || res.Err != "" {
			t.Fatalf("JS: %s", out)
		}
	}

	t.Run("Go logs in to a user created in JS", func(t *testing.T) {
		parse(runScript(t, "user-client.js", peer, "create", "jsuser", "js password"))
		jsPub := res.Pub
		assertJSON(t, runScript(t, "user-client.js", peer, "put", "jsuser", "js password", "profile", `{"name":"From JS"}`), `{"ok":true}`)

		db := goClient()
		u, err := db.Login(ctx, "jsuser", "js password")
		if err != nil {
			t.Fatal(err)
		}
		if u.Pub() != jsPub {
			t.Fatalf("Go got pub %s, JS created %s", u.Pub(), jsPub)
		}
		name, err := db.User(jsPub).Get("profile").Get("name").Once[string](ctx)
		if err != nil || name != "From JS" {
			t.Fatalf("Go read %q, %v", name, err)
		}
		if _, err := db.Login(ctx, "jsuser", "wrong password"); !errors.Is(err, gundb.ErrWrongLogin) {
			t.Fatalf("wrong password: %v", err)
		}
	})

	t.Run("JS logs in to a user created in Go", func(t *testing.T) {
		db := goClient()
		u, err := db.CreateUser(ctx, "gouser", "go password")
		if err != nil {
			t.Fatal(err)
		}
		if err := u.Get("profile").PutAck(ctx, Person{Name: "From Go"}); err != nil {
			t.Fatal(err)
		}
		parse(runScript(t, "user-client.js", peer, "login", "gouser", "go password"))
		if res.Pub != u.Pub() {
			t.Fatalf("JS got pub %s, Go created %s", res.Pub, u.Pub())
		}
		// sea.js verifies the Go signatures before it shows the data.
		assertJSON(t, runScript(t, "user-client.js", peer, "read", u.Pub(), "profile", "name"), `"From Go"`)

		// JS writes as the Go-created user; Go reads it back.
		assertJSON(t, runScript(t, "user-client.js", peer, "put", "gouser", "go password", "status", `{"text":"hi from JS"}`), `{"ok":true}`)
		text, err := goClient().User(u.Pub()).Get("status").Get("text").Once[string](ctx)
		if err != nil || text != "hi from JS" {
			t.Fatalf("Go read %q, %v", text, err)
		}
	})
}

// ---- helpers ----

func requireNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	if _, err := os.Stat("testdata/node_modules/gun"); err != nil {
		t.Skip("run `npm install` in interop/testdata to enable the JS interop tests")
	}
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startJSServer(t *testing.T) string {
	port := freePort(t)
	cmd := exec.Command("node", "server.js", fmt.Sprint(port), t.TempDir())
	cmd.Dir = "testdata"
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if sc.Text() == "READY" {
			go func() {
				for sc.Scan() {
				}
			}()
			return fmt.Sprintf("http://127.0.0.1:%d/gun", port)
		}
	}
	t.Fatal("JS server did not start")
	return ""
}

// startJS runs client.js in the background. It returns once the client
// prints WATCHING (or immediately for other commands); call the returned
// func to wait for its result.
func startJS(t *testing.T, peer string, args ...string) func() string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	cmd := exec.CommandContext(ctx, "node", append([]string{"client.js", peer}, args...)...)
	cmd.Dir = "testdata"
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	result := make(chan string, 1)
	watching := make(chan struct{})
	var once sync.Once
	go func() {
		defer once.Do(func() { close(watching) })
		sc := bufio.NewScanner(out)
		var lines []string
		for sc.Scan() {
			line := sc.Text()
			lines = append(lines, line)
			if line == "WATCHING" {
				once.Do(func() { close(watching) })
			}
			if res, ok := strings.CutPrefix(line, "RESULT "); ok {
				result <- res
				return
			}
		}
		result <- "no result; output:\n" + strings.Join(lines, "\n")
	}()
	if slices.Contains([]string{"watch", "map"}, args[0]) {
		<-watching
	}
	return func() string {
		defer cancel()
		defer cmd.Wait()
		return <-result
	}
}

func runJS(t *testing.T, peer string, args ...string) string {
	t.Helper()
	return startJS(t, peer, args...)()
}

// runScript runs a testdata script and returns what it printed after RESULT.
func runScript(t *testing.T, script string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", append([]string{script}, args...)...)
	cmd.Dir = "testdata"
	out, _ := cmd.CombinedOutput()
	for line := range strings.Lines(string(out)) {
		if r, ok := strings.CutPrefix(strings.TrimSpace(line), "RESULT "); ok {
			return r
		}
	}
	t.Fatalf("%s %q: no result; output:\n%s", script, args, out)
	return ""
}

func assertJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if json.Unmarshal([]byte(got), &g) != nil || json.Unmarshal([]byte(want), &w) != nil {
		t.Fatalf("not JSON:\n got %s\nwant %s", got, want)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Fatalf("\n got %s\nwant %s", gb, wb)
	}
}

func waitFor(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case got := <-ch:
			if got == want {
				return
			}
		case <-timeout:
			t.Fatalf("never got %q", want)
		}
	}
}
