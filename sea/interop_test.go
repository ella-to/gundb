package sea

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// These tests run every primitive against GUN's own sea.js. They skip
// unless Node.js and the JS deps are installed:
//
//	cd interop/testdata && npm install

type nodeSEA struct {
	mu  sync.Mutex
	in  io.WriteCloser
	out *bufio.Scanner
}

var (
	jsOnce sync.Once
	jsSEA  *nodeSEA
	jsErr  error
)

func requireJS(t *testing.T) *nodeSEA {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	if _, err := os.Stat("../interop/testdata/node_modules/gun/sea.js"); err != nil {
		t.Skip("run `npm install` in interop/testdata to enable the SEA interop tests")
	}
	jsOnce.Do(func() {
		cmd := exec.Command("node", "sea-rpc.js")
		cmd.Dir = "../interop/testdata"
		cmd.Stderr = os.Stderr
		in, err := cmd.StdinPipe()
		if err != nil {
			jsErr = err
			return
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			jsErr = err
			return
		}
		if jsErr = cmd.Start(); jsErr != nil {
			return
		}
		sc := bufio.NewScanner(out)
		sc.Buffer(nil, 1<<24)
		jsSEA = &nodeSEA{in: in, out: sc}
	})
	if jsErr != nil {
		t.Fatal(jsErr)
	}
	return jsSEA
}

// call runs one sea.js operation and returns its result as JSON.
func (n *nodeSEA) call(t *testing.T, op string, args map[string]any) json.RawMessage {
	t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	if args == nil {
		args = map[string]any{}
	}
	args["op"] = op
	b, _ := json.Marshal(args)
	if _, err := n.in.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	for n.out.Scan() {
		line := n.out.Bytes()
		if !bytes.HasPrefix(line, []byte(`{"`)) {
			continue // GUN's greeting and other logs
		}
		var res struct {
			R     json.RawMessage
			Err   string
			Undef bool
		}
		if err := json.Unmarshal(line, &res); err != nil {
			t.Fatalf("sea.js: %s", line)
		}
		if res.Err != "" {
			t.Fatalf("sea.js %s: %s", op, res.Err)
		}
		if res.Undef {
			return nil
		}
		return res.R
	}
	t.Fatal("sea.js exited")
	return nil
}

func (n *nodeSEA) str(t *testing.T, op string, args map[string]any) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(n.call(t, op, args), &s); err != nil {
		t.Fatalf("sea.js %s did not return a string", op)
	}
	return s
}

func (n *nodeSEA) pair(t *testing.T) *Pair {
	t.Helper()
	var p Pair
	if err := json.Unmarshal(n.call(t, "pair", nil), &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

func sameJSON(t *testing.T, what string, got, want json.RawMessage) {
	t.Helper()
	var g, w any
	if json.Unmarshal(got, &g) != nil || json.Unmarshal(want, &w) != nil {
		t.Fatalf("%s: not JSON: got %s, want %s", what, got, want)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if !bytes.Equal(gb, wb) {
		t.Fatalf("%s:\n got %s\nwant %s", what, got, want)
	}
}

// Values with the corners of JS's JSON handling: strings that hold JSON,
// unicode, escapes, number formatting, key order.
var samples = []any{
	"hello", "", "123", `{"b":1,"a":2}`, "true", "null", `"quoted"`, "SEA is not this",
	"emoji 😀 and é and \u2028 and \"quotes\" and \\ and <tags>",
	1.5, 0, -2, 1e21, 1e-7, 123456789012, true, false, nil,
	map[string]any{"name": "Alice", "age": 30, "nested": map[string]any{"x": []any{1, "two", nil}}},
	json.RawMessage(`{"z":1,"a":{"y":2,"b":3}}`), // key order must survive
	[]any{"a", 1, false},
}

func TestSignInterop(t *testing.T) {
	js := requireJS(t)
	goPair, err := NewPair()
	if err != nil {
		t.Fatal(err)
	}
	jsPair := js.pair(t)
	for i, data := range samples {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			// Go signs, JS verifies; with a Go key and with a JS key.
			for _, p := range []*Pair{goPair, jsPair} {
				signed, err := Sign(data, p)
				if err != nil {
					t.Fatal(err)
				}
				jsGot := js.call(t, "verify", map[string]any{"data": signed, "pub": p.Pub})
				goGot, err := Verify(signed, p.Pub)
				if err != nil {
					t.Fatalf("Go cannot verify its own %s: %v", signed, err)
				}
				if jsGot == nil {
					t.Fatalf("sea.js rejected %s", signed)
				}
				sameJSON(t, "verified message", goGot, jsGot)

				// JS signs, Go verifies.
				jsSigned := js.str(t, "sign", map[string]any{"data": data, "pair": p})
				goGot, err = Verify(jsSigned, p.Pub)
				if err != nil {
					t.Fatalf("Go rejected sea.js's %s: %v", jsSigned, err)
				}
				sameJSON(t, "verified message", goGot, js.call(t, "verify", map[string]any{"data": jsSigned, "pub": p.Pub}))
			}
		})
	}

	// A wrong key or a changed message must fail on both sides.
	signed, _ := Sign("pay bob 10", goPair)
	if _, err := Verify(signed, jsPair.Pub); err != ErrSignature {
		t.Fatalf("wrong key: %v", err)
	}
	tampered := strings.Replace(signed, "10", "99", 1)
	if _, err := Verify(tampered, goPair.Pub); err != ErrSignature {
		t.Fatalf("tampered: %v", err)
	}
	if js.call(t, "verify", map[string]any{"data": tampered, "pub": goPair.Pub}) != nil {
		t.Fatal("sea.js accepted a tampered message")
	}
}

func TestEncryptInterop(t *testing.T) {
	js := requireJS(t)
	pair := js.pair(t)
	keys := []string{pair.EPriv, "a passphrase", "ünïcødé key"}
	for round := range 20 { // random salts exercise the UTF-8 key derivation
		for _, key := range keys {
			for i, data := range samples {
				enc, err := Encrypt(data, key)
				if err != nil {
					t.Fatal(err)
				}
				want := js.call(t, "decrypt", map[string]any{"data": enc, "key": key})
				if want == nil {
					t.Fatalf("round %d sample %d: sea.js could not decrypt %s", round, i, enc)
				}
				got, err := Decrypt(enc, key)
				if err != nil {
					t.Fatalf("Go cannot decrypt its own: %v", err)
				}
				sameJSON(t, "decrypted", got, want)

				jsEnc := js.str(t, "encrypt", map[string]any{"data": data, "key": key})
				got, err = Decrypt(jsEnc, key)
				if err != nil {
					t.Fatalf("round %d sample %d: Go cannot decrypt sea.js's %s: %v", round, i, jsEnc, err)
				}
				sameJSON(t, "decrypted", got, js.call(t, "decrypt", map[string]any{"data": jsEnc, "key": key}))
			}
		}
	}
	enc, _ := Encrypt("secret", "right")
	if _, err := Decrypt(enc, "wrong"); err != ErrDecrypt {
		t.Fatalf("wrong key: %v", err)
	}
}

func TestSecretInterop(t *testing.T) {
	js := requireJS(t)
	alice, err := NewPair()
	if err != nil {
		t.Fatal(err)
	}
	bob := js.pair(t)
	goSide, err := Secret(bob.EPub, alice)
	if err != nil {
		t.Fatal(err)
	}
	if jsSide := js.str(t, "secret", map[string]any{"epub": alice.EPub, "pair": bob}); goSide != jsSide {
		t.Fatalf("Alice (Go) derived %q, Bob (JS) %q", goSide, jsSide)
	}
	if same := js.str(t, "secret", map[string]any{"epub": bob.EPub, "pair": alice}); goSide != same {
		t.Fatalf("Go %q, JS with the same keys %q", goSide, same)
	}
}

func TestWorkAndHashInterop(t *testing.T) {
	js := requireJS(t)
	for _, c := range [][2]string{{"password", "salt"}, {"pässwörd 😀", "x9Kd0aPq"}, {"", "s"}} {
		got, err := Work(c[0], c[1])
		if err != nil {
			t.Fatal(err)
		}
		if want := js.str(t, "work", map[string]any{"data": c[0], "salt": c[1]}); got != want {
			t.Fatalf("Work(%q, %q) = %s, sea.js %s", c[0], c[1], got, want)
		}
	}
	for _, data := range samples {
		got, err := Hash(data)
		if err != nil {
			t.Fatal(err)
		}
		if want := js.str(t, "hash", map[string]any{"data": data}); got != want {
			t.Fatalf("Hash(%v) = %s, sea.js %s", data, got, want)
		}
	}
}
