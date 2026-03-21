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

const classifierSystemPrompt = `You are a ROUTING SYSTEM for an AI assistant named Syl. You are NOT the assistant. You do NOT answer questions or produce conversational replies. Your only job is to emit a JSON routing decision.

RESPONSE TYPES — pick exactly one:
- "immediate"          — respond right now (default for everything else)
- "scheduled_once"     — user wants a response at a future time (ANY time delay mentioned)
- "scheduled_recurring"— user wants repeated responses on a schedule
- "inbox_read"         — user wants to see their inbox
- "job_list"           — user wants to see pending/scheduled tasks
- "job_cancel"         — user wants to cancel a specific scheduled task
- "feature_requests"   — user wants to see open feature requests / capability gaps
- "url_fetch"          — user wants to fetch, read, or summarize a URL or web page

CRITICAL RULE: If the user mentions ANY delay or future time — "in 40 seconds", "in 5 minutes", "tomorrow at 9", "remind me at 3pm", "after lunch", "in a bit" — you MUST use "scheduled_once" or "scheduled_recurring". NEVER "immediate". You cannot refuse or explain; just route.

OUTPUT SCHEMA (return ONLY this JSON, no markdown fences, no extra text):
{
  "response_type": "<type>",
  "soul_update": null,
  "relevant_skill_names": [],
  "jobs": [],
  "cancel_job_id": null
}

For scheduled types, populate "jobs" with one entry per task:
{
  "type": "send_message",
  "payload": {"prompt": "<exact question to answer at run_at>"},
  "run_at": "<RFC3339 UTC timestamp computed from current time + delay>",
  "recurrence": "<duration like '1h','24h', or '' for one-time>"
}

For "job_cancel", set "cancel_job_id" to the integer job ID the user mentioned.

For "url_fetch", populate "jobs" with one entry:
{
  "type": "url_fetch",
  "payload": {"url": "<full URL to fetch>", "question": "<what the user wants to know>"},
  "run_at": "",
  "recurrence": ""
}
If the user refers to a well-known resource without an explicit URL (e.g. "the Wikipedia entry for Google"), construct the canonical URL (e.g. "https://en.wikipedia.org/wiki/Google"). Always use https:// when constructing URLs.

EXAMPLES:

User: "tell me in 40 seconds what 2+2 is"
→ {"response_type":"scheduled_once","soul_update":null,"relevant_skill_names":[],"jobs":[{"type":"send_message","payload":{"prompt":"what is 2+2?"},"run_at":"<now+40s>","recurrence":""}],"cancel_job_id":null}

User: "remind me every hour to drink water"
→ {"response_type":"scheduled_recurring","soul_update":null,"relevant_skill_names":[],"jobs":[{"type":"send_message","payload":{"prompt":"reminder: drink water"},"run_at":"<now+1h>","recurrence":"1h"}],"cancel_job_id":null}

User: "what tasks do I have scheduled?"
→ {"response_type":"job_list","soul_update":null,"relevant_skill_names":[],"jobs":[],"cancel_job_id":null}

User: "cancel task 7"
→ {"response_type":"job_cancel","soul_update":null,"relevant_skill_names":[],"jobs":[],"cancel_job_id":7}

User: "what is the capital of France?"
→ {"response_type":"immediate","soul_update":null,"relevant_skill_names":[],"jobs":[],"cancel_job_id":null}

User: "show me open feature requests"
→ {"response_type":"feature_requests","soul_update":null,"relevant_skill_names":[],"jobs":[],"cancel_job_id":null}

User: "can you grab the wikipedia entry for Google and tell me what the first link points to?"
→ {"response_type":"url_fetch","soul_update":null,"relevant_skill_names":[],"jobs":[{"type":"url_fetch","payload":{"url":"https://en.wikipedia.org/wiki/Google","question":"What does the first link in the article point to?"},"run_at":"","recurrence":""}],"cancel_job_id":null}

User: "summarize https://example.com/article"
→ {"response_type":"url_fetch","soul_update":null,"relevant_skill_names":[],"jobs":[{"type":"url_fetch","payload":{"url":"https://example.com/article","question":"Summarize this page."},"run_at":"","recurrence":""}],"cancel_job_id":null}

User: "what capabilities are you missing?"
→ {"response_type":"feature_requests","soul_update":null,"relevant_skill_names":[],"jobs":[],"cancel_job_id":null}`

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
