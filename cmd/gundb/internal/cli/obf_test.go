package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"ella.to/gundb"
)

// rawNode reads the node soul from the server as it is stored, with a
// client of its own.
func rawNode(t *testing.T, addr, soul string) *gundb.Node {
	t.Helper()
	db := gundb.New(gundb.Options{Peers: []string{addr}})
	defer db.Close()
	n, err := db.Get(soul).Node(t.Context())
	if err != nil {
		t.Fatalf("read %s: %v", soul, err)
	}
	return n
}

func TestObfuscatedScopes(t *testing.T) {
	addr := startServer(t)
	ali := []string{"--addr", addr, "-u", "ali", "-p", "secret123"}
	bob := []string{"--addr", addr, "-u", "bob", "-p", "secret456"}
	carol := []string{"--addr", addr, "-u", "carol", "-p", "secret789"}
	for _, u := range [][]string{ali, bob, carol} {
		must(t, append([]string{"user", "create"}, u...)...)
	}
	as := func(user []string, args ...string) string { t.Helper(); return must(t, append(args, user...)...) }
	fails := func(user []string, want string, args ...string) {
		t.Helper()
		if out, stderr, err := run(t, "", append(args, user...)...); err == nil {
			t.Fatalf("%v succeeded: %s", args, out)
		} else {
			wantOut(t, stderr, want)
		}
	}

	// The first write creates the scope; names and value are hidden.
	wantOut(t, as(ali, "put", "~/vault/bank", "IBAN DE89", "--obf", "--enc"), "ok  ~/vault/bank = (encrypted)")
	wantOut(t, as(ali, "get", "~/vault", "--obf", "--dec"), `"bank": "IBAN DE89"`)
	out, stderr, err := run(t, "", append([]string{"get", "~/vault", "--obf"}, ali...)...)
	if err != nil || strings.Contains(out, "IBAN") {
		t.Fatalf("without --dec: %s %s %v", out, stderr, err)
	}
	wantOut(t, stderr, "--dec")

	// What the server holds: no names, and nothing pointing at the scope.
	var scopes map[string]struct{ Soul string }
	if err := json.Unmarshal([]byte(as(ali, "get", "~", "--obf")), &scopes); err != nil || scopes["vault"].Soul == "" {
		t.Fatalf("scopes %v, %v", scopes, err)
	}
	root := scopes["vault"].Soul
	stored := rawNode(t, addr, root)
	for k, v := range stored.Fields {
		if strings.Contains(k, "bank") || strings.Contains(string(v.(gundb.String)), "IBAN") {
			t.Fatalf("the server stores %s = %v", k, v)
		}
	}
	wantOut(t, as(ali, "get", "~/vault", "--obf", "--links"), `"bank": {`, `"field": "`, `"#": "`+root+`"`)
	if out := must(t, "get", "~@ali", "--addr", addr); strings.Contains(out, "vault") || strings.Contains(out, root) {
		t.Fatalf("ali's node shows the scope: %s", out)
	}

	// Without --obf, a write to the scope would store names in clear.
	fails(ali, "~/vault is obfuscated", "put", "~/vault/pin", "1234")

	// Nobody else reads it until ali shares it.
	fails(bob, "no access to an obfuscated ~@ali/vault", "get", "~@ali/vault/bank", "--obf", "--dec")
	wantOut(t, as(ali, "put", "~/vault", "--obf", "--to", "bob"), "bob can read ~/vault")
	wantOut(t, as(bob, "get", "~@ali/vault/bank", "--obf", "--dec"), `"IBAN DE89"`)
	fails(carol, "no access", "get", "~@ali/vault/bank", "--obf", "--dec")
	fails(bob, "rejected the write", "put", "~@ali/vault/bank", "mine now", "--obf")

	// Nested nodes and lists.
	as(ali, "put", "~/vault/cards", `{"visa":"4111"}`, "--obf")
	wantOut(t, as(bob, "get", "~@ali/vault/cards/visa", "--obf"), `"4111"`)
	wantOut(t, as(ali, "set", "~/vault/todo", `{"t":"renew passport"}`, "--obf"), "ok  ~/vault/todo/")
	wantOut(t, as(bob, "get", "~@ali/vault/todo", "--obf"), "renew passport")
	if out := must(t, "get", "~@ali", "--addr", addr); strings.Contains(out, "cards") || strings.Contains(out, "todo") {
		t.Fatalf("names leaked into ali's node: %s", out)
	}

	// A new key: bob loses access, carol gets it, the old root is cleared,
	// and the encrypted value is encrypted again with the new key.
	wantOut(t, as(ali, "put", "~/vault", "--obf", "--rotate", "--to", "carol"), "ok  rotated ~/vault: new key", "dropped")
	fails(bob, "no access", "get", "~@ali/vault/bank", "--obf", "--dec")
	wantOut(t, as(carol, "get", "~@ali/vault/bank", "--obf", "--dec"), `"IBAN DE89"`)
	wantOut(t, as(ali, "get", "~/vault/cards/visa", "--obf"), `"4111"`)
	for k, v := range rawNode(t, addr, root).Fields {
		if _, deleted := v.(gundb.Null); !deleted {
			t.Fatalf("the old root still holds %s = %v", k, v)
		}
	}
	if err := json.Unmarshal([]byte(as(ali, "get", "~", "--obf")), &scopes); err != nil || scopes["vault"].Soul == root {
		t.Fatalf("scope root after rotating: %v, %v", scopes, err)
	}
}

func TestObfNeedsAUserSpace(t *testing.T) {
	addr := startServer(t)
	ali := []string{"--addr", addr, "-u", "ali", "-p", "secret123"}
	must(t, append([]string{"user", "create"}, ali...)...)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"put", "notes/1", "x", "--obf"}, "works in a user's space"},
		{[]string{"put", "~/vault", "--obf", "--rotate"}, "no obfuscated scope ~/vault"},
		{[]string{"get", "~/nothing/x", "--obf"}, "no obfuscated scope ~/nothing"},
	} {
		if _, stderr, err := run(t, "", append(tc.args, ali...)...); err == nil {
			t.Fatalf("%v succeeded", tc.args)
		} else {
			wantOut(t, stderr, tc.want)
		}
	}
	if _, stderr, err := run(t, "", "get", "~/vault/x", "--obf", "--addr", addr); err == nil {
		t.Fatal("--obf without a login succeeded")
	} else {
		wantOut(t, stderr, "not logged in")
	}
}
