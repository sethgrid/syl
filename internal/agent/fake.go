package agent

import (
	"database/sql"
	"fmt"
	"sync"
	"time"
)

type FakeStore struct {
	mu     sync.Mutex
	agents map[int64]*Agent
	byName map[string]int64
	byFP   map[string]int64
	nextID int64
}

func NewFakeStore() *FakeStore {
	return &FakeStore{
		agents: make(map[int64]*Agent),
		byName: make(map[string]int64),
		byFP:   make(map[string]int64),
		nextID: 1,
	}
}

func (f *FakeStore) Resolve(name, fingerprint string) (*Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if name != "" {
		if id, ok := f.byName[name]; ok {
			return f.agents[id], nil
		}
	}
	if id, ok := f.byFP[fingerprint]; ok {
		return f.agents[id], nil
	}

	id := f.nextID
	f.nextID++
	a := &Agent{
		ID:          id,
		Fingerprint: fingerprint,
		CreatedAt:   time.Now(),
	}
	if name != "" {
		a.Name = sql.NullString{String: name, Valid: true}
		f.byName[name] = id
	}
	f.byFP[fingerprint] = id
	f.agents[id] = a
	return a, nil
}

func (f *FakeStore) Get(agentID int64) (*Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[agentID]
	if !ok {
		return nil, fmt.Errorf("agent not found: %d", agentID)
	}
	return a, nil
}

func (f *FakeStore) UpdateSoul(agentID int64, soul string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[agentID]
	if !ok {
		return fmt.Errorf("agent not found: %d", agentID)
	}
	a.Soul = soul
	return nil
}
