package classifier

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sethgrid/syl/internal/claude"
)

// ClaudeClassifier uses Claude to pre-classify user messages.
type ClaudeClassifier struct {
	cl      claude.Client
	nowFunc func() time.Time
}

func NewClaudeClassifier(cl claude.Client, nowFunc func() time.Time) *ClaudeClassifier {
	return &ClaudeClassifier{cl: cl, nowFunc: nowFunc}
}

const classifierSystemPrompt = `You are a message classifier for an AI assistant named Syl.
Classify each user message into one of these response types:
- "immediate": respond right away
- "scheduled_once": schedule a one-time future response
- "scheduled_recurring": schedule a recurring response
- "inbox_read": user wants to read their inbox

Return ONLY a JSON object with these fields:
{
  "response_type": "immediate" | "scheduled_once" | "scheduled_recurring" | "inbox_read",
  "soul_update": null or string,
  "relevant_skill_names": [],
  "jobs": []
}

For scheduled jobs, include in "jobs":
{
  "type": "send_message",
  "payload": {"prompt": "the message to send"},
  "run_at": "RFC3339 timestamp",
  "recurrence": "1h" | "24h" | "" (empty string for one-time)
}

Return valid JSON only. No markdown fences. No explanation.`

func (c *ClaudeClassifier) Classify(ctx context.Context, _ string, _ []string, skillNames []string, userMessage string) (*Result, error) {
	now := c.nowFunc().UTC()
	prompt := fmt.Sprintf("Current UTC time: %s\nAvailable skills: %s\nUser message: %s",
		now.Format(time.RFC3339),
		strings.Join(skillNames, ", "),
		userMessage)

	msgs := []claude.Message{{Role: "user", Content: prompt}}
	resp, err := c.cl.Complete(ctx, classifierSystemPrompt, msgs)
	if err != nil {
		return &Result{ResponseType: "immediate"}, nil
	}

	// Strip markdown fences if present.
	resp = strings.TrimSpace(resp)
	if strings.HasPrefix(resp, "```") {
		lines := strings.Split(resp, "\n")
		if len(lines) > 2 {
			resp = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	var result Result
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		return &Result{ResponseType: "immediate"}, nil
	}
	return &result, nil
}
