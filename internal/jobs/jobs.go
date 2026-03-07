package jobs

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type Job struct {
	ID         int64
	AgentID    int64
	JobType    string
	Payload    json.RawMessage
	Status     string
	RunAt      time.Time
	LockedAt   sql.NullTime
	Recurrence string
	CreatedAt  time.Time
}

// Store is the interface for job persistence.
type Store interface {
	Enqueue(agentID int64, jobType string, payload any, runAt time.Time, recurrence string) (*Job, error)
	FetchDue() (*Job, error)
	MarkRunning(jobID int64) error
	MarkDone(jobID int64) error
	MarkFailed(jobID int64) error
	ResetStuck(threshold time.Duration) (int, error)
	ListPending(agentID int64) ([]*Job, error)
	Cancel(jobID int64) error
	Close() error
}

// Processor handles a single job.
type Processor interface {
	Process(job *Job) error
}

// Runner polls for due jobs and dispatches them.
type Runner struct {
	store        Store
	processor    Processor
	logger       *slog.Logger
	pollInterval time.Duration
	closeCh      chan struct{}
	wg           sync.WaitGroup
}

func NewRunner(store Store, processor Processor, logger *slog.Logger, pollInterval time.Duration) *Runner {
	return &Runner{
		store:        store,
		processor:    processor,
		logger:       logger,
		pollInterval: pollInterval,
		closeCh:      make(chan struct{}),
	}
}

func (r *Runner) Start() {
	go r.resetLoop()
	r.pollLoop()
}

func (r *Runner) Close() error {
	close(r.closeCh)
	r.wg.Wait()
	return r.store.Close()
}

func (r *Runner) pollLoop() {
	for {
		select {
		case <-r.closeCh:
			return
		default:
		}
		job, err := r.store.FetchDue()
		if err != nil {
			r.logger.Error("fetch due job", "error", err)
			time.Sleep(r.pollInterval)
			continue
		}
		if job == nil {
			time.Sleep(r.pollInterval)
			continue
		}
		if err := r.store.MarkRunning(job.ID); err != nil {
			r.logger.Error("mark running", "job_id", job.ID, "error", err)
			continue
		}
		r.wg.Add(1)
		go func(j *Job) {
			defer r.wg.Done()
			if err := r.processor.Process(j); err != nil {
				r.logger.Error("process job", "job_id", j.ID, "job_type", j.JobType, "error", err)
				_ = r.store.MarkFailed(j.ID)
				return
			}
			_ = r.store.MarkDone(j.ID)
		}(job)
	}
}

func (r *Runner) resetLoop() {
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.closeCh:
			return
		case <-ticker.C:
			n, err := r.store.ResetStuck(5 * time.Minute)
			if err != nil {
				r.logger.Error("reset stuck jobs", "error", err)
			} else if n > 0 {
				r.logger.Info("reset stuck jobs", "count", n)
			}
		}
	}
}

// SQLiteStore implements Store against SQLite.
type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(db *sql.DB) *SQLiteStore {
	return &SQLiteStore{db: db}
}

func (s *SQLiteStore) Enqueue(agentID int64, jobType string, payload any, runAt time.Time, recurrence string) (*Job, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	res, err := s.db.Exec(
		`INSERT INTO jobs (agent_id, job_type, payload, run_at, recurrence) VALUES (?, ?, ?, ?, ?)`,
		agentID, jobType, string(data), runAt, recurrence)
	if err != nil {
		return nil, fmt.Errorf("insert job: %w", err)
	}
	id, _ := res.LastInsertId()
	return &Job{ID: id, AgentID: agentID, JobType: jobType, Payload: data, Status: "pending", RunAt: runAt, Recurrence: recurrence}, nil
}

func (s *SQLiteStore) FetchDue() (*Job, error) {
	row := s.db.QueryRow(
		`SELECT id, agent_id, job_type, payload, status, run_at, locked_at, recurrence, created_at
		 FROM jobs WHERE status = 'pending' AND run_at <= datetime('now')
		 ORDER BY run_at ASC LIMIT 1`)
	return scanJob(row)
}

func (s *SQLiteStore) MarkRunning(jobID int64) error {
	_, err := s.db.Exec(
		`UPDATE jobs SET status = 'running', locked_at = datetime('now') WHERE id = ?`, jobID)
	return err
}

func (s *SQLiteStore) MarkDone(jobID int64) error {
	_, err := s.db.Exec(`UPDATE jobs SET status = 'done' WHERE id = ?`, jobID)
	return err
}

func (s *SQLiteStore) MarkFailed(jobID int64) error {
	_, err := s.db.Exec(`UPDATE jobs SET status = 'failed' WHERE id = ?`, jobID)
	return err
}

func (s *SQLiteStore) ResetStuck(threshold time.Duration) (int, error) {
	res, err := s.db.Exec(
		`UPDATE jobs SET status = 'pending', locked_at = NULL
		 WHERE status = 'running' AND locked_at < datetime('now', ?)`,
		fmt.Sprintf("-%d seconds", int(threshold.Seconds())))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *SQLiteStore) ListPending(agentID int64) ([]*Job, error) {
	rows, err := s.db.Query(
		`SELECT id, agent_id, job_type, payload, status, run_at, locked_at, recurrence, created_at
		 FROM jobs WHERE agent_id = ? AND status = 'pending' ORDER BY run_at ASC`,
		agentID)
	if err != nil {
		return nil, fmt.Errorf("list pending: %w", err)
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		var j Job
		var payload string
		if err := rows.Scan(&j.ID, &j.AgentID, &j.JobType, &payload, &j.Status, &j.RunAt, &j.LockedAt, &j.Recurrence, &j.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		j.Payload = json.RawMessage(payload)
		out = append(out, &j)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) Cancel(jobID int64) error {
	res, err := s.db.Exec(`UPDATE jobs SET status = 'failed' WHERE id = ? AND status = 'pending'`, jobID)
	if err != nil {
		return fmt.Errorf("cancel job: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("job not found or already completed")
	}
	return nil
}

func (s *SQLiteStore) Close() error { return nil }

func scanJob(row *sql.Row) (*Job, error) {
	var j Job
	var payload string
	if err := row.Scan(&j.ID, &j.AgentID, &j.JobType, &payload, &j.Status, &j.RunAt, &j.LockedAt, &j.Recurrence, &j.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	j.Payload = json.RawMessage(payload)
	return &j, nil
}
