package senderkey

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"ella.to/gundb/sea"
)

func newPair(t *testing.T) *sea.Pair {
	t.Helper()
	p, err := sea.NewPair()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func send(t *testing.T, s *Sender, n int) []Message {
	t.Helper()
	var out []Message
	for i := range n {
		m, err := s.Encrypt(fmt.Sprintf("msg %d", i))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func read(t *testing.T, r *Receiver, m Message) string {
	t.Helper()
	raw, err := r.Decrypt(m)
	if err != nil {
		t.Fatalf("decrypt %d: %v", m.N, err)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRoundTripInOrder(t *testing.T) {
	s, _ := NewSender()
	r := NewReceiver(s.Share())
	for i, m := range send(t, s, 5) {
		if got, want := read(t, r, m), fmt.Sprintf("msg %d", i); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	if len(r.Skipped) != 0 {
		t.Fatalf("kept %d skipped keys", len(r.Skipped))
	}
}

func TestEveryMessageHasItsOwnKey(t *testing.T) {
	s, _ := NewSender()
	mk0, _ := step(s.Key.Seed)
	a, _ := s.Encrypt("same")
	b, _ := s.Encrypt("same")
	if _, err := sea.Decrypt(a.Data, mk0); err != nil {
		t.Fatal("message 0 doesn't open with its key")
	}
	if _, err := sea.Decrypt(b.Data, mk0); err == nil {
		t.Fatal("message 1 opened with message 0's key")
	}
}

func TestOutOfOrder(t *testing.T) {
	s, _ := NewSender()
	r := NewReceiver(s.Share())
	ms := send(t, s, 4)
	read(t, r, ms[2]) // 0 and 1 are skipped and kept
	if len(r.Skipped) != 2 {
		t.Fatalf("skipped %d keys, want 2", len(r.Skipped))
	}
	read(t, r, ms[0])
	read(t, r, ms[3])
	read(t, r, ms[1])
	if len(r.Skipped) != 0 {
		t.Fatalf("kept %d skipped keys", len(r.Skipped))
	}
}

func TestMessageOpensOnce(t *testing.T) {
	s, _ := NewSender()
	r := NewReceiver(s.Share())
	ms := send(t, s, 2)
	read(t, r, ms[0])
	read(t, r, ms[1])
	if _, err := r.Decrypt(ms[0]); !errors.Is(err, ErrOld) {
		t.Fatalf("second decrypt: %v, want ErrOld", err)
	}
}

func TestLateJoinerCannotReadHistory(t *testing.T) {
	s, _ := NewSender()
	before := send(t, s, 3)
	late := NewReceiver(s.Share()) // shared at message 3
	after := send(t, s, 1)
	for _, m := range before {
		if _, err := late.Decrypt(m); !errors.Is(err, ErrOld) {
			t.Fatalf("message %d: %v, want ErrOld", m.N, err)
		}
	}
	read(t, late, after[0])
}

// TestStolenStateCannotReadThePast is forward secrecy: a copy of a
// receiver's saved state, taken after it read some messages, can't decrypt
// those messages again.
func TestStolenStateCannotReadThePast(t *testing.T) {
	s, _ := NewSender()
	r := NewReceiver(s.Share())
	ms := send(t, s, 3)
	read(t, r, ms[0])
	read(t, r, ms[1])

	saved, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var stolen Receiver
	if err := json.Unmarshal(saved, &stolen); err != nil {
		t.Fatal(err)
	}
	for _, m := range ms[:2] {
		if _, err := stolen.Decrypt(m); !errors.Is(err, ErrOld) {
			t.Fatalf("stolen state read message %d: %v", m.N, err)
		}
	}
	read(t, &stolen, ms[2]) // it does read what comes after
}

func TestSenderStateRoundTrips(t *testing.T) {
	s, _ := NewSender()
	r := NewReceiver(s.Share())
	send(t, s, 1)
	saved, _ := json.Marshal(s)
	var back Sender
	if err := json.Unmarshal(saved, &back); err != nil {
		t.Fatal(err)
	}
	m, err := back.Encrypt("after restart")
	if err != nil {
		t.Fatal(err)
	}
	if m.N != 1 {
		t.Fatalf("restored sender at %d, want 1", m.N)
	}
	if got := read(t, r, m); got != "after restart" {
		t.Fatal(got)
	}
}

func TestTooFarAhead(t *testing.T) {
	s, _ := NewSender()
	r := NewReceiver(s.Share())
	s.Key.N += MaxSkip + 1 // pretend the sender sent that many
	m, _ := s.Encrypt("far")
	if _, err := r.Decrypt(m); !errors.Is(err, ErrTooFar) {
		t.Fatalf("got %v, want ErrTooFar", err)
	}
}

func TestBadMessageChangesNothing(t *testing.T) {
	s, _ := NewSender()
	r := NewReceiver(s.Share())
	ms := send(t, s, 3)
	forged := ms[2]
	forged.Data = ms[1].Data // wrong ciphertext at a later position
	if _, err := r.Decrypt(forged); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("got %v, want ErrDecrypt", err)
	}
	if r.Key.N != 0 || len(r.Skipped) != 0 {
		t.Fatalf("state moved: n=%d skipped=%d", r.Key.N, len(r.Skipped))
	}
	for _, m := range ms {
		read(t, r, m)
	}
}

func TestOtherChain(t *testing.T) {
	a, _ := NewSender()
	b, _ := NewSender()
	m, _ := b.Encrypt("x")
	if _, err := NewReceiver(a.Share()).Decrypt(m); !errors.Is(err, ErrChain) {
		t.Fatalf("got %v, want ErrChain", err)
	}
}

func TestSealOpen(t *testing.T) {
	prekey, other := newPair(t), newPair(t)
	s, _ := NewSender()
	send(t, s, 2)
	sealed, err := Seal(s.Share(), prekey.EPub)
	if err != nil {
		t.Fatal(err)
	}
	k, err := Open(sealed, prekey)
	if err != nil {
		t.Fatal(err)
	}
	if k.Chain != s.Key.Chain || k.N != 2 {
		t.Fatalf("opened %+v", k)
	}
	m, _ := s.Encrypt("hello")
	if got := read(t, NewReceiver(k), m); got != "hello" {
		t.Fatal(got)
	}

	if _, err := Open(sealed, other); err == nil {
		t.Fatal("opened with another prekey")
	}
	wrong := sealed
	wrong.PreKey = other.EPub
	if _, err := Open(wrong, other); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("got %v, want ErrDecrypt", err)
	}
}

// TestMessageIsPlainSEA checks the format a GUN.js client would rely on: the
// data is a SEA ciphertext that sea.Decrypt opens with the message key.
func TestMessageIsPlainSEA(t *testing.T) {
	s, _ := NewSender()
	seed := append([]byte(nil), s.Key.Seed...)
	m, _ := s.Encrypt(map[string]any{"text": "hi"})
	mk, _ := step(seed)
	raw, err := sea.Decrypt(m.Data, mk)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"text":"hi"}` {
		t.Fatalf("got %s", raw)
	}
}
