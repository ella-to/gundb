package gundb

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ella.to/gundb/sea"
)

// signedPut returns a put message for soul in ali's space: the fields in
// signed signed by ali at state, plus the fields in forged as they are.
func signedPut(t *testing.T, ali *User, id, soul string, state float64, signed map[string]string, forged map[string]Value, forgedState float64) []byte {
	t.Helper()
	g := graph{}
	for k, v := range signed {
		g.node(soul).Set(k, String(v), state)
	}
	if _, err := ali.sign(g); err != nil {
		t.Fatal(err)
	}
	for k, v := range forged {
		g.node(soul).Set(k, v, forgedState)
	}
	raw, err := (&message{ID: id, Put: g}).appendWire(nil)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// waitFor returns the first frame on c that contains s.
func waitFor(t *testing.T, c *scriptConn, s string) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f := <-c.sent:
			if strings.Contains(string(f), s) {
				return string(f)
			}
		case <-deadline:
			t.Fatalf("no frame with %s", s)
		}
	}
}

// A signed field older than the stored one is not verified, so a put can
// carry a forged one; it must be neither stored nor passed on, while the
// rest of the put is.
func TestStaleSignedFieldsAreDropped(t *testing.T) {
	ctx := t.Context()
	relay := newDB(t)
	ali, err := relay.CreateUser(ctx, "ali", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if err := ali.Get("profile").Put(ctx, map[string]any{"name": "Ali"}); err != nil {
		t.Fatal(err)
	}
	soul := "~" + ali.Pub() + "/profile"

	sub := newScriptConn()
	go relay.Attach(t.Context(), sub)
	writer := newScriptConn()
	go relay.Attach(t.Context(), writer)
	eventually(t, func() bool { return len(relay.Peers()) == 2 })
	sub.recv <- []byte(`{"#":"s1","get":{"#":"` + soul + `"}}`)
	waitFor(t, sub, `"@":"s1"`)

	// name: forged, but older than Ali's. bio: new and signed by ali.
	writer.recv <- signedPut(t, ali, "w1", soul, relay.state.Next(),
		map[string]string{"bio": "hello"}, map[string]Value{"name": String("Mallory")}, 1)
	if ack := waitFor(t, writer, `"@":"w1"`); strings.Contains(ack, "err") {
		t.Fatalf("put refused: %s", ack)
	}
	got := waitFor(t, sub, `"bio"`)
	if strings.Contains(got, "Mallory") {
		t.Fatalf("forged field passed on: %s", got)
	}
	if name, _ := relay.User(ali.Pub()).Get("profile").Get("name").Once[string](ctx); name != "Ali" {
		t.Fatalf("name is %q", name)
	}
	if bio, _ := relay.User(ali.Pub()).Get("profile").Get("bio").Once[string](ctx); bio != "hello" {
		t.Fatalf("bio is %q", bio)
	}

	// The same forged field newer than Ali's is refused, as before.
	writer.recv <- signedPut(t, ali, "w2", soul, 0, nil, map[string]Value{"name": String("Mallory")}, relay.state.Next())
	if ack := waitFor(t, writer, `"@":"w2"`); !strings.Contains(ack, "unverified") {
		t.Fatalf("newer forged field accepted: %s", ack)
	}
}

// racyStore makes the first read of a node say a field is newer than it
// is, so that the check before the merge sees it lose and the merge sees
// it win: the field must then be verified during the merge.
type racyStore struct {
	FieldReader
	mu     sync.Mutex
	fooled bool
}

func (s *racyStore) GetFields(ctx context.Context, soul string, fields []string) (*Node, error) {
	n, err := s.FieldReader.GetFields(ctx, soul, fields)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fooled || err != nil {
		return n, err
	}
	s.fooled = true
	if n == nil {
		n = NewNode(soul)
	}
	for _, f := range fields {
		n.Set(f, String("newer"), 1e15)
	}
	return n, nil
}

func TestFieldThatWinsLateIsVerified(t *testing.T) {
	ctx := t.Context()
	db := New(Options{Store: &racyStore{FieldReader: NewMemoryStore().(FieldReader)}})
	defer db.Close()
	pair, err := sea.NewPair()
	if err != nil {
		t.Fatal(err)
	}
	soul := "~" + pair.Pub + "/profile"
	g := graph{}
	g.node(soul).Set("name", String("Mallory"), db.state.Next())
	if _, err := db.apply(ctx, g, nil); !errors.Is(err, ErrUnverified) {
		t.Fatalf("unverified field stored: %v", err)
	}
	if n, _ := db.store.Get(ctx, soul); n != nil {
		t.Fatalf("stored %+v", n)
	}
}

// A reply the relay passes on to whoever asked is verified whole, stale
// fields included: a relay never passes on data that might be forged.
func TestRelayedRepliesAreVerifiedWhole(t *testing.T) {
	ctx := t.Context()
	relay := newDB(t)
	ali, err := relay.CreateUser(ctx, "ali", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if err := ali.Get("profile").Put(ctx, map[string]any{"name": "Ali"}); err != nil {
		t.Fatal(err)
	}
	soul := "~" + ali.Pub() + "/profile"
	asker := newScriptConn()
	go relay.Attach(t.Context(), asker)
	upstream := newScriptConn()
	go relay.Attach(t.Context(), upstream)
	eventually(t, func() bool { return len(relay.Peers()) == 2 })

	// The relay has the node but not "bio", so it asks its other peers.
	asker.recv <- []byte(`{"#":"q1","get":{"#":"` + soul + `",".":"bio"}}`)
	var getID string
	for getID == "" {
		var m struct {
			ID  string          `json:"#"`
			Get json.RawMessage `json:"get"`
		}
		if json.Unmarshal(<-upstream.sent, &m) == nil && strings.Contains(string(m.Get), "bio") {
			getID = m.ID
		}
	}
	reply := signedPut(t, ali, "r1", soul, relay.state.Next(),
		map[string]string{"bio": "hello"}, map[string]Value{"name": String("Mallory")}, 1)
	reply = []byte(strings.Replace(string(reply), `"#":"r1"`, `"#":"r1","@":"`+getID+`"`, 1))
	upstream.recv <- reply
	got := waitFor(t, asker, `"@":"q1"`)
	if strings.Contains(got, "Mallory") || strings.Contains(got, "hello") {
		t.Fatalf("relayed a reply with a forged field: %s", got)
	}
}
