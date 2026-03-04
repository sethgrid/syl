package inbox

import (
	"database/sql"
	"fmt"
	"sync"
	"time"
)

type FakeStore struct {
	mu    sync.Mutex
	items []Item
	next  int64
}

func NewFakeStore() *FakeStore {
	return &FakeStore{next: 1}
}

func (f *FakeStore) Add(question string) (*Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it := Item{ID: f.next, Question: question, Status: "open", CreatedAt: time.Now()}
	f.next++
	f.items = append(f.items, it)
	return &it, nil
}

func (f *FakeStore) List() ([]Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Item, len(f.items))
	copy(out, f.items)
	return out, nil
}

func (f *FakeStore) ListOpen() ([]Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Item
	for _, it := range f.items {
		if it.Status == "open" {
			out = append(out, it)
		}
	}
	return out, nil
}

func (f *FakeStore) Answer(id int64, answer string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, it := range f.items {
		if it.ID == id {
			f.items[i].Answer = sql.NullString{String: answer, Valid: true}
			f.items[i].Status = "answered"
			return nil
		}
	}
	return fmt.Errorf("inbox item not found: %d", id)
}
