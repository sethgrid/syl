package classifier_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sethgrid/syl/internal/classifier"
	"github.com/sethgrid/syl/internal/claude"
)

func TestClaudeClassifier_Immediate(t *testing.T) {
	resp, _ := json.Marshal(classifier.Result{ResponseType: "immediate"})
	fake := &claude.FakeClient{Tokens: []string{string(resp)}}
	clf := classifier.NewClaudeClassifier(fake, time.Now)

	result, err := clf.Classify(context.Background(), "", nil, nil, "hello")
	require.NoError(t, err)
	assert.Equal(t, "immediate", result.ResponseType)
}

func TestClaudeClassifier_FallbackOnBadJSON(t *testing.T) {
	fake := &claude.FakeClient{Tokens: []string{"not valid json at all"}}
	clf := classifier.NewClaudeClassifier(fake, time.Now)

	result, err := clf.Classify(context.Background(), "", nil, nil, "hello")
	require.NoError(t, err)
	assert.Equal(t, "immediate", result.ResponseType)
}

func TestClaudeClassifier_ScheduledOnce(t *testing.T) {
	runAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	resp, _ := json.Marshal(classifier.Result{
		ResponseType: "scheduled_once",
		Jobs: []classifier.JobSpec{
			{
				Type:    "send_message",
				Payload: map[string]any{"prompt": "remind me"},
				RunAt:   runAt,
			},
		},
	})
	fake := &claude.FakeClient{Tokens: []string{string(resp)}}
	clf := classifier.NewClaudeClassifier(fake, time.Now)

	result, err := clf.Classify(context.Background(), "", nil, nil, "remind me in an hour")
	require.NoError(t, err)
	assert.Equal(t, "scheduled_once", result.ResponseType)
	require.Len(t, result.Jobs, 1)
	assert.Equal(t, "send_message", result.Jobs[0].Type)
}
