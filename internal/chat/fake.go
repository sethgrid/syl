package chat

import (
	"sync"
	"time"
)

type FakeStore struct {
	mu   sync.Mutex
	msgs []Message
	next int64
}

func NewFakeStore() *FakeStore {
	return &FakeStore{next: 1}
}

func (f *FakeStore) Add(agentID int64, role, content string) (*Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := Message{
		ID:        f.next,
		AgentID:   agentID,
		Role:      role,
		Content:   content,
		CreatedAt: time.Now(),
	}
	f.next++
	f.msgs = append(f.msgs, m)
	return &m, nil
}

func (f *FakeStore) Recent(agentID int64, limit int) ([]Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Message
	for _, m := range f.msgs {
		if m.AgentID == agentID {
			out = append(out, m)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}
