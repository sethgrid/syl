package classifier

import (
	"context"
)

// Result is the output of the pre-classifier call.
type Result struct {
	SoulUpdate         *string   `json:"soul_update"`
	ResponseType       string    `json:"response_type"` // "immediate", "scheduled_once", "scheduled_recurring", "inbox_read", "job_list", "job_cancel", "feature_requests", "url_fetch"
	Jobs               []JobSpec `json:"jobs"`
	RelevantSkillNames []string  `json:"relevant_skill_names"`
	CancelJobID        *int64    `json:"cancel_job_id"` // populated for job_cancel response type
}

// JobSpec describes a job to enqueue.
type JobSpec struct {
	Type       string         `json:"type"`
	Payload    map[string]any `json:"payload"`
	RunAt      string         `json:"run_at"`      // RFC3339, empty means now
	Recurrence string         `json:"recurrence"`  // "1h", "24h", etc.; empty = once
}

// Classifier determines how to handle a user message.
type Classifier interface {
	Classify(ctx context.Context, soul string, history []string, skillNames []string, userMessage string) (*Result, error)
}

// FakeClassifier always returns an immediate response for testing.
type FakeClassifier struct {
	Result *Result
	Err    error
}

func (f *FakeClassifier) Classify(_ context.Context, _ string, _ []string, _ []string, _ string) (*Result, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	if f.Result != nil {
		return f.Result, nil
	}
	return &Result{ResponseType: "immediate"}, nil
}
