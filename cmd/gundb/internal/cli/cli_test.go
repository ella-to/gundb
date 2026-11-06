package cli

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"ella.to/gundb"
)

// buffer is a bytes.Buffer safe for the goroutines of sub and server.
type buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// startServer runs gundb server on a free port and returns its address.
// It stops when the test ends.
func startServer(t *testing.T, args ...string) string {
	t.Helper()
	addr, stop := startServerStoppable(t, args...)
	t.Cleanup(stop)
	return addr
}

func startServerStoppable(t *testing.T, args ...string) (addr string, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	logs := &buffer{}
	a := &App{Stdin: strings.NewReader(""), Stdout: logs, Stderr: logs, onListen: func(s string) { ready <- s }}
	done := make(chan error, 1)
	go func() {
		done <- a.Command().Run(ctx, append([]string{"gundb", "server", "--addr", "http://127.0.0.1:0/gun"}, args...))
	}()
	select {
	case addr = <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("server did not start: %v\n%s", err, logs)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("server did not start in time")
	}
	stopped := false
	return addr, func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			t.Errorf("server: %v\n%s", err, logs)
		}
	}
}

// run runs gundb with args and returns what it printed. err is the error
// Main would print; stderr already holds it, with its hints.
func run(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return runCtx(t.Context(), stdin, args...)
}

func runCtx(ctx context.Context, stdin string, args ...string) (stdout, stderr string, err error) {
	var out, errw buffer
	a := &App{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errw}
	if err = a.Command().Run(ctx, append([]string{"gundb"}, args...)); err != nil {
		printError(&errw, err)
	}
	return out.String(), errw.String(), err
}

// must runs gundb and fails the test if it fails.
func must(t *testing.T, args ...string) string {
	t.Helper()
	out, stderr, err := run(t, "", args...)
	if err != nil {
		t.Fatalf("gundb %s: %v\n%s", strings.Join(args, " "), err, stderr)
	}
	return out
}

func wantOut(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("output %q does not contain %q", got, w)
		}
	}
}

func TestPutGetSet(t *testing.T) {
	addr := startServer(t)
	wantOut(t, must(t, "put", "users/1/1", "hello world", "--addr="+addr), `ok  users/1/1 = "hello world"`)
	wantOut(t, must(t, "put", "users/2", `{"name":"Ali","age":30}`, "--addr", addr), `users/2 = {"age":30,"name":"Ali"}`)
	wantOut(t, must(t, "set", "users", "2", "--addr", addr), "ok  users/", " = 2")

	wantOut(t, must(t, "get", "users/1", "--addr", addr), `"1": "hello world"`)
	wantOut(t, must(t, "get", "users/2/age", "--addr", addr), "30")
	all := must(t, "get", "users", "--addr", addr)
	wantOut(t, all, `"name": "Ali"`, `"1": "hello world"`, ": 2")
}

func TestValues(t *testing.T) {
	addr := startServer(t)
	must(t, "put", "v/n", "30", "--addr", addr)
	must(t, "put", "v/s", "30", "--string", "--addr", addr)
	must(t, "put", "v/b", "true", "--addr", addr)
	must(t, "put", "v/q", `"quoted"`, "--addr", addr)
	if _, stderr, err := run(t, `{"from":"stdin"}`, "put", "v/in", "-", "--addr", addr); err != nil {
		t.Fatal(stderr)
	}
	out := must(t, "get", "v", "--addr", addr)
	wantOut(t, out, `"n": 30`, `"s": "30"`, `"b": true`, `"q": "quoted"`, `"from": "stdin"`)

	// Deleting with null.
	must(t, "put", "v/n", "null", "--addr", addr)
	if out := must(t, "get", "v", "--addr", addr); strings.Contains(out, `"n"`) {
		t.Errorf("v/n not deleted: %s", out)
	}

	_, stderr, err := run(t, "", "put", "v/list", "[1,2]", "--addr", addr)
	if err == nil {
		t.Fatal("an array was accepted")
	}
	wantOut(t, stderr, "cannot store a JSON array", "hint:")
}

func TestErrorsAreExplained(t *testing.T) {
	addr := startServer(t)
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"get", "nothing/here", "--addr", addr}, []string{"error: nothing at nothing/here", "hint:"}},
		{[]string{"get", "users//1", "--addr", addr}, []string{`path "users//1" has an empty key`}},
		{[]string{"put", "a", "hello", "world"}, []string{"too many arguments", "'hello world'", "usage: gundb put <path> <value>"}},
		{[]string{"get"}, []string{"needs 1 argument", "usage: gundb get <path>"}},
		{[]string{"get", "x", "--header", "nope", "--addr", addr}, []string{`bad --header "nope"`, "Name=Value"}},
		{[]string{"get", "x", "--bogus"}, []string{"not defined", "see: gundb get --help"}},
		{[]string{"get", "x", "--addr", "localhost"}, []string{`bad --addr "localhost"`}},
		{[]string{"put", "~/x", "1", "--addr", addr}, []string{"your own user space", "--username"}},
		{[]string{"put", "x", "1", "--addr", addr}, []string{"x is a top-level node", "gundb put x/name 1"}},
		{[]string{"put", "x/k", "1", "--enc", "--addr", addr}, []string{"not logged in"}},
		{[]string{"put", "x/k", "1", "--to", "bob", "--addr", addr}, []string{"--to only works with --enc"}},
		{[]string{"get", "x", "--verify", "--addr", addr}, []string{"whose signature"}},
		{[]string{"user", "create", "--addr", addr}, []string{"needs --username and --password"}},
		{[]string{"get", "x", "-u", "ali", "--addr", addr}, []string{"needs a --password"}},
		{[]string{"server", "--store", "redis"}, []string{`unknown --store "redis"`}},
		{[]string{"server", "--addr", "https://localhost:1/gun"}, []string{"does not do TLS"}},
		{[]string{"server", "--peers", "nope"}, []string{`bad --peers "nope"`}},
	} {
		_, stderr, err := run(t, "", tc.args...)
		if err == nil {
			t.Errorf("gundb %s succeeded", strings.Join(tc.args, " "))
			continue
		}
		wantOut(t, stderr, tc.want...)
	}
}

func TestUnreachableServerFailsFast(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "http://" + l.Addr().String() + "/gun"
	l.Close() // nothing listens there now

	start := time.Now()
	_, stderr, err := run(t, "", "get", "x", "--addr", addr, "--timeout", "10s")
	if err == nil {
		t.Fatal("get succeeded")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %s to give up on a refused connection", took)
	}
	wantOut(t, stderr, "nothing is listening at "+addr, "gundb server")
}

// --timeout 0 waits for a server that is not up yet.
func TestTimeoutZeroWaitsForTheServer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := l.Addr().String()
	l.Close()

	type result struct {
		stderr string
		err    error
	}
	got := make(chan result, 1)
	go func() {
		_, stderr, err := runCtx(t.Context(), "", "get", "x", "--addr", "http://"+host+"/gun", "--timeout", "0")
		got <- result{stderr, err}
	}()
	time.Sleep(300 * time.Millisecond)
	startServer(t, "--addr", "http://"+host+"/gun")
	select {
	case r := <-got:
		// It connected: the answer is about the data, not the connection.
		wantOut(t, r.stderr, "waiting for the server", "nothing at x")
	case <-time.After(10 * time.Second):
		t.Fatal("get never connected")
	}
}

func TestTimeout(t *testing.T) {
	// A server that accepts the TCP connection but never upgrades it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	start := time.Now()
	_, stderr, err := run(t, "", "get", "x", "--addr", "http://"+l.Addr().String()+"/gun", "--timeout", "300ms")
	if err == nil {
		t.Fatal("get succeeded")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("--timeout 300ms took %s", took)
	}
	wantOut(t, stderr, "timed out after 300ms", "--timeout 0")
}

func TestHeaders(t *testing.T) {
	relay := gundb.New()
	defer relay.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer 1234" || r.Header.Get("X-Team") != "a,b" {
			http.Error(w, "who are you?", http.StatusUnauthorized)
			return
		}
		relay.ServeHTTP(w, r)
	}))
	defer srv.Close()
	addr := srv.URL + "/gun"

	_, stderr, err := run(t, "", "put", "x/k", "1", "--addr", addr)
	if err == nil {
		t.Fatal("put without the header succeeded")
	}
	wantOut(t, stderr, "refused the connection (401 Unauthorized: who are you?)", "--header")

	must(t, "put", "x/k", "1", "--addr", addr, "--header", "Authorization=Bearer 1234", "-H", "X-Team: a,b")
	wantOut(t, must(t, "get", "x/k", "--addr", addr, "-H", "Authorization=Bearer 1234", "-H", "X-Team=a,b"), "1")
}

func TestSub(t *testing.T) {
	addr := startServer(t)
	must(t, "put", "todos/1/title", "first", "--addr", addr)

	ctx, stop := context.WithCancel(t.Context())
	var out, errw buffer
	done := make(chan error, 1)
	go func() {
		a := &App{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errw}
		done <- a.Command().Run(ctx, []string{"gundb", "sub", "todos", "--map", "--addr", addr})
	}()
	eventually(t, func() bool { return strings.Contains(out.String(), `1  {"title":"first"}`) })
	must(t, "put", "todos/2/title", "second", "--addr", addr)
	eventually(t, func() bool { return strings.Contains(out.String(), `2  {"title":"second"}`) })
	must(t, "put", "todos/1/title", "changed", "--addr", addr)
	eventually(t, func() bool { return strings.Contains(out.String(), `1  {"title":"changed"}`) })

	stop() // Ctrl-C
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sub ended with %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sub did not stop")
	}
	wantOut(t, errw.String(), "watching todos (Ctrl-C to stop)")

	// Without --map, the whole value, one line per change.
	ctx, stop = context.WithCancel(t.Context())
	defer stop()
	var whole buffer
	go func() {
		a := &App{Stdin: strings.NewReader(""), Stdout: &whole, Stderr: &errw}
		a.Command().Run(ctx, []string{"gundb", "sub", "todos/2", "--addr", addr})
	}()
	eventually(t, func() bool { return strings.Contains(whole.String(), `{"title":"second"}`) })
}

func TestUsersSignAndEncrypt(t *testing.T) {
	addr := startServer(t)
	ali := []string{"--addr", addr, "-u", "ali", "-p", "secret123"}
	bob := []string{"--addr", addr, "-u", "bob", "-p", "secret456"}
	carol := []string{"--addr", addr, "-u", "carol", "-p", "secret789"}

	out := must(t, append([]string{"user", "create"}, ali...)...)
	wantOut(t, out, "created user ali", "pub ", "epub ")
	must(t, append([]string{"user", "create"}, bob...)...)
	must(t, append([]string{"user", "create"}, carol...)...)
	if _, stderr, err := run(t, "", append([]string{"user", "create"}, ali...)...); err == nil {
		t.Error("ali created twice")
	} else {
		wantOut(t, stderr, "that username is taken")
	}
	if _, stderr, err := run(t, "", "user", "login", "--addr", addr, "-u", "ali", "-p", "wrong-password"); err == nil {
		t.Error("logged in with a wrong password")
	} else {
		wantOut(t, stderr, "wrong username or password")
	}
	wantOut(t, must(t, append([]string{"user", "login", "--keys"}, ali...)...), `"priv":`, `"epriv":`)

	// Ali's own space: written by her, readable by anyone, not writable
	// by Bob.
	must(t, append([]string{"put", "~/profile/name", "Ali"}, ali...)...)
	wantOut(t, must(t, "get", "~@ali/profile/name", "--addr", addr), `"Ali"`)
	if _, stderr, err := run(t, "", append([]string{"put", "~@ali/profile/name", "Mallory"}, bob...)...); err == nil {
		t.Error("bob wrote to ali's space")
	} else {
		wantOut(t, stderr, "rejected the write")
	}

	// Encrypted for herself: unreadable without --dec.
	must(t, append([]string{"put", "~/notes/1", "my secret", "--enc"}, ali...)...)
	out, stderr, err := run(t, "", append([]string{"get", "~/notes/1"}, ali...)...)
	if err != nil || strings.Contains(out, "my secret") {
		t.Fatalf("without --dec: %s %s %v", out, stderr, err)
	}
	wantOut(t, stderr, "--dec")
	wantOut(t, must(t, append([]string{"get", "~/notes", "--dec"}, ali...)...), `"1": "my secret"`)

	// Signed and encrypted for Bob, outside any user space.
	must(t, append([]string{"put", "inbox/bob", "hi Bob", "--enc", "--to", "bob", "--sign"}, ali...)...)
	wantOut(t, must(t, append([]string{"get", "inbox/bob", "--dec", "--from", "ali"}, bob...)...), `"hi Bob"`)
	if _, stderr, err := run(t, "", append([]string{"get", "inbox/bob", "--dec", "--from", "ali"}, carol...)...); err == nil {
		t.Error("carol decrypted bob's message")
	} else {
		wantOut(t, stderr, "cannot decrypt inbox/bob")
	}
	if _, stderr, err := run(t, "", append([]string{"get", "inbox/bob", "--dec", "--from", "carol"}, bob...)...); err == nil {
		t.Error("a message from ali passed as carol's")
	} else {
		wantOut(t, stderr, "was not signed by carol")
	}

	// Signed only: anyone can check it, no login needed.
	must(t, append([]string{"put", "news/1", `{"t":"hello"}`, "--sign"}, ali...)...)
	wantOut(t, must(t, "get", "news/1", "--verify", "--from", "ali", "--addr", addr), `"t": "hello"`)
	if _, _, err := run(t, "", "get", "news/1", "--verify", "--from", "bob", "--addr", addr); err == nil {
		t.Error("ali's signature passed as bob's")
	}
	if _, _, err := run(t, "", "get", "x", "--verify", "--from", "nobody", "--addr", addr); err == nil {
		t.Error("an unknown user was found")
	}
}

func TestServerPeers(t *testing.T) {
	first := startServer(t)
	second := startServer(t, "--peers", first+",http://127.0.0.1:1/gun") // one peer down
	must(t, "put", "shared/k", "from the second", "--addr", second)
	eventually(t, func() bool {
		out, _, err := run(t, "", "get", "shared/k", "--addr", first)
		return err == nil && strings.Contains(out, "from the second")
	})
}

func TestServerPebbleKeepsData(t *testing.T) {
	dir := t.TempDir()
	addr, stop := startServerStoppable(t, "--data", dir) // --data implies pebble
	must(t, "put", "kept/k", "still here", "--addr", addr)
	stop()

	addr = startServer(t, "--store", "pebble", "--data", dir)
	wantOut(t, must(t, "get", "kept/k", "--addr", addr), "still here")

	_, stderr, err := run(t, "", "server", "--store", "memory", "--data", dir)
	if err == nil {
		t.Fatal("memory with --data started")
	}
	wantOut(t, stderr, "--data is for --store pebble")
}

func TestListenAddr(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:8080/gun": "localhost:8080 /gun",
		"localhost:8080":            "localhost:8080 /gun",
		":9000":                     ":9000 /gun",
		"http://0.0.0.0/relay":      "0.0.0.0:8765 /relay",
		"ws://127.0.0.1:1234/gun":   "127.0.0.1:1234 /gun",
	} {
		host, path, err := listenAddr(in)
		if err != nil || host+" "+path != want {
			t.Errorf("listenAddr(%q) = %q %q %v, want %s", in, host, path, err, want)
		}
	}
}

func TestParseHeaders(t *testing.T) {
	h, err := parseHeaders([]string{"Authorization=Bearer a=b", "X-Team: a,b", "Cookie=s=1; t=2"})
	if err != nil {
		t.Fatal(err)
	}
	if h.Get("Authorization") != "Bearer a=b" || h.Get("X-Team") != "a,b" || h.Get("Cookie") != "s=1; t=2" {
		t.Errorf("headers = %v", h)
	}
}

func TestUnexpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	for in, want := range map[string]string{
		home + "/notes/1": "~/notes/1",
		home:              "~",
		"~/notes":         "~/notes",
		"users/1":         "users/1",
	} {
		if got := unexpandHome(in); got != want {
			t.Errorf("unexpandHome(%q) = %q, want %q", in, got, want)
		}
	}
}

func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLinks(t *testing.T) {
	addr := startServer(t)
	a := "--addr=" + addr
	must(t, "put", "users/1", `{"name":"Ali"}`, a)
	must(t, "put", "users/2", `{"name":"Bob","city":"Berlin"}`, a)

	// put --link: one link, followed by get, shown as stored by --links.
	wantOut(t, must(t, "put", "users/1/best", "--link", "users/2", a), "ok  users/1/best links to users/2")
	wantOut(t, must(t, "get", "users/1/best/city", a), "Berlin")
	wantOut(t, must(t, "get", "users/1", "--links", a), `"#": "users/1"`, `"best": {`, `"#": "users/2"`)

	// Writing through the link changes the linked node.
	must(t, "put", "users/1/best/city", "Madrid", a)
	wantOut(t, must(t, "get", "users/2/city", a), "Madrid")

	// set --link: a list of links, keyed by soul. A set item has a random
	// soul, which is resolved from its path.
	item := strings.Fields(must(t, "set", "groups", `{"name":"Go"}`, a))[1] // groups/<soul>
	soul := strings.TrimPrefix(item, "groups/")
	must(t, "set", "users/1/friends", "--link", "users/2", a)
	wantOut(t, must(t, "set", "users/1/groups", "--link", item, a), "now has a link to "+item+" (soul "+soul+")")
	must(t, "set", "users/1/groups", "--link", soul, a) // the soul itself works too, and does not duplicate
	wantOut(t, must(t, "get", "users/1", a), `"name": "Bob"`, `"name": "Go"`)
	nodes := must(t, "get", "users/1/groups", "--links", a)
	if strings.Count(nodes, `"#"`) != 2 { // its own soul, and the one link
		t.Errorf("groups: %s", nodes)
	}

	// unset removes the link and leaves the node.
	wantOut(t, must(t, "unset", "users/1/friends", "--link", "users/2", a), "no longer links to users/2")
	if out := must(t, "get", "users/1/friends", a); strings.Contains(out, "Bob") {
		t.Errorf("friend not removed: %s", out)
	}
	wantOut(t, must(t, "get", "users/2/name", a), "Bob")

	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"put", "users/1/x", "--link", "users/9", a}, []string{"nothing at users/9 to link to"}},
		{[]string{"put", "users/1/x", "--link", "users/2/name", a}, []string{"users/2/name is a value, not a node", "--link users/2"}},
		{[]string{"put", "users/1/x", "v", "--link", "users/2", a}, []string{"takes the place of the value"}},
		{[]string{"put", "users", "--link", "users/2", a}, []string{"top-level node", "gundb set users --link users/2"}},
		{[]string{"put", "users/1/x", "--link", "users/2", "--enc", a}, []string{"--enc does not work with --link"}},
		{[]string{"unset", "users/1/friends", a}, []string{"unset needs --link"}},
		{[]string{"unset", "users/1/friends", "--link", "users/2", a}, []string{"users/1/friends does not link to users/2"}},
		{[]string{"get", "users/1/name", "--links", a}, []string{"is a value, not a node"}},
	} {
		_, stderr, err := run(t, "", tc.args...)
		if err == nil {
			t.Errorf("gundb %s succeeded", strings.Join(tc.args, " "))
			continue
		}
		wantOut(t, stderr, tc.want...)
	}

	// unset also removes the link a path write leaves in the parent
	// (users.2 from put users/2/name); users/2 then has no path, only its
	// soul.
	wantOut(t, must(t, "unset", "users", "--link", "users/2", a), "no longer links to users/2")
	if out := must(t, "get", "users", "--links", a); strings.Contains(out, `"users/2"`) {
		t.Errorf("path link not removed: %s", out)
	}
}

func TestGroupChat(t *testing.T) {
	addr := startServer(t)
	dir := t.TempDir()
	as := func(name string, args ...string) []string {
		return append(args, "--addr", addr, "--dir", dir, "-u", name, "-p", name+" password")
	}
	for _, name := range []string{"alice", "bob", "carol", "dave"} {
		must(t, "user", "create", "-u", name, "-p", name+" password", "--addr", addr)
	}
	// Bob can't be added before he sets up this device.
	must(t, as("alice", "group", "init")...)
	must(t, as("alice", "group", "create", "team")...)
	_, stderr, err := run(t, "", as("alice", "group", "add", "team", "bob")...)
	if err == nil {
		t.Fatal("added a member without group keys")
	}
	wantOut(t, stderr, "bob has not set up group chats yet", "gundb group init -u bob")

	must(t, as("bob", "group", "init")...)
	must(t, as("carol", "group", "init")...)
	wantOut(t, must(t, as("alice", "group", "add", "team", "bob", "carol")...), "added bob, carol", "alice/team")
	wantOut(t, must(t, as("bob", "group", "members", "alice/team")...), "alice  (owner)", "bob", "carol")

	must(t, as("alice", "group", "send", "team", "hi team")...)
	must(t, as("bob", "group", "send", "alice/team", "hello alice")...)
	wantOut(t, must(t, as("carol", "group", "read", "alice/team")...), "alice: hi team", "bob: hello alice")
	// Read again: the history is on the device, though each key is gone.
	wantOut(t, must(t, as("carol", "group", "read", "alice/team")...), "alice: hi team", "bob: hello alice")
	if out := must(t, as("carol", "group", "read", "alice/team", "--new")...); out != "" {
		t.Errorf("--new printed old messages: %q", out)
	}

	// Dave joins late: he reads from now on, not the history.
	must(t, as("dave", "group", "init")...)
	must(t, as("alice", "group", "add", "team", "dave")...)
	must(t, as("alice", "group", "send", "team", "welcome dave")...)
	out := must(t, as("dave", "group", "read", "alice/team")...)
	wantOut(t, out, "alice: welcome dave")
	if strings.Contains(out, "hi team") {
		t.Errorf("dave read the history: %s", out)
	}

	// Carol watches; she sees new messages as they arrive.
	ctx, cancel := context.WithCancel(t.Context())
	var subOut, subErr buffer
	done := make(chan error, 1)
	go func() {
		a := &App{Stdin: strings.NewReader(""), Stdout: &subOut, Stderr: &subErr}
		done <- a.Command().Run(ctx, append([]string{"gundb"}, as("carol", "group", "sub", "alice/team")...))
	}()
	waitFor(t, &subErr, "watching")
	must(t, as("bob", "group", "send", "alice/team", "live from bob")...)
	waitFor(t, &subOut, "bob: live from bob")
	cancel()
	<-done

	// Carol is removed: everyone's next message is on a new chain.
	wantOut(t, must(t, as("alice", "group", "remove", "team", "carol")...), "removed carol")
	must(t, as("bob", "group", "send", "alice/team", "carol can't read this")...)
	wantOut(t, must(t, as("alice", "group", "read", "team")...), "bob: carol can't read this")
	_, stderr, err = run(t, "", as("carol", "group", "read", "alice/team")...)
	if err == nil {
		t.Fatal("a removed member read the group")
	}
	wantOut(t, stderr, "you are not a member of alice/team")

	// Bob's password on another device (no keys there) reads nothing.
	_, stderr, err = run(t, "", "group", "read", "alice/team", "--addr", addr, "--dir", t.TempDir(), "-u", "bob", "-p", "bob password")
	if err == nil {
		t.Fatal("read without the device's keys")
	}
	wantOut(t, stderr, "this device has no group keys for bob", "gundb group init")
}

// waitFor waits until b contains want.
func waitFor(t *testing.T, b *buffer, want string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if strings.Contains(b.String(), want) {
			return
		}
	}
	t.Fatalf("no %q in %q", want, b.String())
}
