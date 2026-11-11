package gundb

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// syncStore is a SyncStore in memory that counts its syncs and remembers
// which writes were synced.
type syncStore struct {
	FieldReader
	mu       sync.Mutex
	unsynced int // writes not synced yet
	syncs    int
	synced   int // writes made durable
}

func newSyncStore() *syncStore { return &syncStore{FieldReader: NewMemoryStore().(FieldReader)} }

func (s *syncStore) PutBatch(ctx context.Context, changed []*Node) error {
	if err := s.PutBatchUnsynced(ctx, changed); err != nil {
		return err
	}
	return s.Sync(ctx)
}

func (s *syncStore) PutBatchUnsynced(ctx context.Context, changed []*Node) error {
	for _, n := range changed {
		if err := s.PutFields(ctx, n.Soul, n); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.unsynced += len(changed)
	s.mu.Unlock()
	return nil
}

func (s *syncStore) Sync(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncs++
	s.synced += s.unsynced
	s.unsynced = 0
	return nil
}

func (s *syncStore) counts() (syncs, synced, unsynced int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncs, s.synced, s.unsynced
}

// The puts of one frame are synced once, and acked after that.
func TestFrameOfPutsSharesOneSync(t *testing.T) {
	store := newSyncStore()
	db := New(Options{Store: store})
	defer db.Close()
	conn := newScriptConn()
	go db.Attach(t.Context(), conn)
	eventually(t, func() bool { return len(db.Peers()) == 1 })

	var frame []string
	for _, id := range []string{"a", "b", "c"} {
		frame = append(frame, `{"#":"`+id+`","put":{"n/`+id+`":{"_":{"#":"n/`+id+`",">":{"v":1}},"v":1}}}`)
	}
	conn.recv <- []byte("[" + strings.Join(frame, ",") + "]")
	for acked := 0; acked < 3; {
		f := waitFor(t, conn, `"@":"`)
		// Acks are sent after the sync: by then every write is durable.
		if syncs, synced, unsynced := store.counts(); syncs != 1 || synced != 3 || unsynced != 0 {
			t.Fatalf("ack before the sync: %d syncs, %d synced, %d not", syncs, synced, unsynced)
		}
		acked += strings.Count(f, `"ok":{"":1}`)
	}
}

// A local write returns once it is durable.
func TestLocalWriteIsSynced(t *testing.T) {
	store := newSyncStore()
	db := New(Options{Store: store})
	defer db.Close()
	if err := db.Get("a").Put(t.Context(), map[string]any{"x": 1, "child": map[string]any{"y": 2}}); err != nil {
		t.Fatal(err)
	}
	if syncs, synced, unsynced := store.counts(); syncs != 1 || synced != 2 || unsynced != 0 {
		t.Fatalf("%d syncs, %d synced, %d not", syncs, synced, unsynced)
	}
}
