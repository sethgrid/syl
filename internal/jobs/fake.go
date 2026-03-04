package jobs

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type FakeStore struct {
	mu   sync.Mutex
	jobs []*Job
	next int64
}

func NewFakeStore() *FakeStore {
	return &FakeStore{next: 1}
}

func (f *FakeStore) Enqueue(agentID int64, jobType string, payload any, runAt time.Time) (*Job, error) {
	data, _ := json.Marshal(payload)
	f.mu.Lock()
	defer f.mu.Unlock()
	j := &Job{
		ID:        f.next,
		AgentID:   agentID,
		JobType:   jobType,
		Payload:   data,
		Status:    "pending",
		RunAt:     runAt,
		CreatedAt: time.Now(),
	}
	f.next++
	f.jobs = append(f.jobs, j)
	return j, nil
}

func (f *FakeStore) FetchDue() (*Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for _, j := range f.jobs {
		if j.Status == "pending" && !j.RunAt.After(now) {
			return j, nil
		}
	}
	return nil, nil
}

func (f *FakeStore) MarkRunning(jobID int64) error {
	return f.setStatus(jobID, "running")
}

func (f *FakeStore) MarkDone(jobID int64) error {
	return f.setStatus(jobID, "done")
}

func (f *FakeStore) MarkFailed(jobID int64) error {
	return f.setStatus(jobID, "failed")
}

func (f *FakeStore) ResetStuck(_ time.Duration) (int, error) { return 0, nil }

func (f *FakeStore) Close() error { return nil }

func (f *FakeStore) setStatus(jobID int64, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, j := range f.jobs {
		if j.ID == jobID {
			j.Status = status
			return nil
		}
	}
	return fmt.Errorf("job not found: %d", jobID)
}
