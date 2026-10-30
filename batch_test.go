package gundb

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
)

// batchStore is the memory store with PutBatch, counting its calls.
type batchStore struct {
	*memoryStore
	mu      sync.Mutex
	batches [][]string // souls of each PutBatch
}

func (s *batchStore) PutBatch(ctx context.Context, changed []*Node) error {
	var souls []string
	for _, n := range changed {
		if err := s.PutFields(ctx, n.Soul, n); err != nil {
			return err
		}
		souls = append(souls, n.Soul)
	}
	slices.Sort(souls)
	s.mu.Lock()
	s.batches = append(s.batches, souls)
	s.mu.Unlock()
	return nil
}

func TestSetIsOneBatch(t *testing.T) {
	ctx := t.Context()
	store := &batchStore{memoryStore: NewMemoryStore().(*memoryStore)}
	db := New(Options{Store: store})
	t.Cleanup(func() { db.Close() })

	item, err := db.Get("todos").Set(ctx, Todo{Title: "milk"})
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]string{slices.Sorted(slices.Values([]string{item.Key(), "todos"}))}; !slices.EqualFunc(store.batches, want, slices.Equal) {
		t.Fatalf("batches %v, want %v", store.batches, want)
	}
	all, err := db.Get("todos").Once[map[string]Todo](ctx)
	if err != nil || all[item.Key()].Title != "milk" {
		t.Fatalf("list: %v, %v", all, err)
	}

	// HAM still decides in a batch: an older write of both nodes loses.
	g := graph{}
	g.node("todos").Set(item.Key(), Null{}, 1)
	g.node(item.Key()).Set("title", String("old"), 1)
	if _, err := db.apply(ctx, g, nil); err != nil {
		t.Fatal(err)
	}
	if all, _ := db.Get("todos").Once[map[string]Todo](ctx); all[item.Key()].Title != "milk" {
		t.Fatalf("an older batch won: %v", all)
	}
}

// TestBatchesDontDeadlock runs many multi-node writes over shared souls at
// once; each takes several merge locks.
func TestBatchesDontDeadlock(t *testing.T) {
	ctx := t.Context()
	store := &batchStore{memoryStore: NewMemoryStore().(*memoryStore)}
	db := New(Options{Store: store})
	t.Cleanup(func() { db.Close() })
	var wg sync.WaitGroup
	for w := range 16 {
		wg.Go(func() {
			for i := range 200 {
				g := graph{}
				state := db.state.Next()
				for k := range 5 { // souls shared between writers, in varying order
					g.node(fmt.Sprint("s", (w+i+k*7)%23)).Set("v", Number(w), state)
				}
				if _, err := db.apply(ctx, g, nil); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
}
