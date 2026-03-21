package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sethgrid/syl/internal/agent"
	"github.com/sethgrid/syl/internal/chat"
	"github.com/sethgrid/syl/internal/classifier"
	"github.com/sethgrid/syl/internal/claude"
	"github.com/sethgrid/syl/internal/inbox"
	"github.com/sethgrid/syl/internal/jobs"
	"github.com/sethgrid/syl/internal/skills"
	"github.com/sethgrid/syl/internal/sse"
	"github.com/sethgrid/syl/logger"
	"github.com/sethgrid/syl/web"
)

func handleIndex() http.HandlerFunc {
	data, err := web.FS.ReadFile("index.html")
	if err != nil {
		panic("web/index.html missing from embed: " + err.Error())
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	}
}

func handleSession(agents agent.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fp := r.URL.Query().Get("fingerprint")
		if fp == "" {
			http.Error(w, "fingerprint required", http.StatusBadRequest)
			return
		}
		ag, err := agents.Resolve(r.URL.Query().Get("name"), fp)
		if err != nil {
			logger.FromRequest(r).Error("resolve agent", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]int64{"agent_id": ag.ID}); err != nil {
			logger.FromRequest(r).Error("encode session", "error", err)
		}
	}
}

func handleHealthcheck() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "OK")
	}
}

func handleStatus(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{
			"version": version,
			"status":  "ok",
		}); err != nil {
			logger.FromRequest(r).Error("encode status", "error", err)
		}
	}
}

func handleSSE(broker *sse.Broker, activeConns prometheus.Gauge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID, err := strconv.ParseInt(r.URL.Query().Get("agent_id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid agent_id", http.StatusBadRequest)
			return
		}
		activeConns.Inc()
		defer activeConns.Dec()
		broker.Subscribe(agentID, w, r)
	}
}

type messageReq struct {
	Text        string `json:"text"`
	AgentName   string `json:"agent_name"`
	Fingerprint string `json:"fingerprint"`
}

// handleChat is a synchronous variant of handleMessage for testing and CI.
// No goroutine, no SSE, no classifier, no jobs — just a blocking Claude call.
func handleChat(
	agents agent.Store,
	chats chat.Store,
	cl claude.Client,
	sk *skills.Loader,
	historyLimit int,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log := logger.FromRequest(r)
		var req messageReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Fingerprint == "" {
			http.Error(w, "fingerprint required", http.StatusBadRequest)
			return
		}
		ag, err := agents.Resolve(req.AgentName, req.Fingerprint)
		if err != nil {
			log.Error("resolve agent", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if _, err := chats.Add(ag.ID, "user", req.Text); err != nil {
			log.Error("add chat message", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		history := compactionAwareHistory(r.Context(), log, chats, ag.ID, historyLimit)
		response, err := cl.Complete(r.Context(), buildSystemPrompt(agentDisplayName(ag), ag.Soul, sk, nil), buildMessages(history))
		if err != nil {
			log.Error("claude complete", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if response != "" {
			if _, err := chats.Add(ag.ID, "assistant", response); err != nil {
				log.Error("persist assistant message", "error", err)
				// non-fatal — response already computed
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"response": response})
	}
}

// handleMessage is the main message ingestion endpoint.
// Pipeline: resolve agent → persist message → classify → stream Claude response via SSE.
func handleMessage(
	agents agent.Store,
	chats chat.Store,
	broker *sse.Broker,
	clf classifier.Classifier,
	cl claude.Client,
	sk *skills.Loader,
	inbx inbox.Store,
	jobStore jobs.Store,
	compactionThreshold int,
	asyncTimeout time.Duration,
	historyLimit int,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log := logger.FromRequest(r)

		var req messageReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Fingerprint == "" {
			http.Error(w, "fingerprint required", http.StatusBadRequest)
			return
		}

		// 1. Resolve agent.
		ag, err := agents.Resolve(req.AgentName, req.Fingerprint)
		if err != nil {
			log.Error("resolve agent", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// 2. Persist user message.
		if _, err := chats.Add(ag.ID, "user", req.Text); err != nil {
			log.Error("add message", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusAccepted)

		// 3. Run pipeline asynchronously so HTTP response is returned immediately.
		// Use a fresh context with a hard deadline — r.Context() is canceled when the
		// handler returns, but context.Background() with no deadline risks goroutine leaks
		// if Claude hangs.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), asyncTimeout)
			defer cancel()

			publish := func(evt sse.Event) {
				if err := broker.Publish(ag.ID, evt); err != nil {
					log.Error("publish event", "type", evt.Type, "error", err)
				}
			}
			persistAssistant := func(content string) {
				if _, err := chats.Add(ag.ID, "assistant", content); err != nil {
					log.Error("persist assistant message", "error", err)
				}
			}

			// 4. Pre-classify.
			history := compactionAwareHistory(ctx, log, chats, ag.ID, historyLimit)
			result, err := clf.Classify(ctx, ag.Soul, history, sk.Names(), req.Text)
			if err != nil {
				log.Error("classify", "error", err)
				publish(sse.Event{Type: "error", Content: "classification failed"})
				return
			}

			// 5. Apply soul update if present.
			if result.SoulUpdate != nil && *result.SoulUpdate != "" {
				if err := agents.UpdateSoul(ag.ID, *result.SoulUpdate); err != nil {
					log.Error("update soul", "error", err)
				}
			}

			// 6. Enqueue scheduled jobs if any.
			for _, js := range result.Jobs {
				if _, err := jobStore.Enqueue(ag.ID, js.Type, js.Payload, parseRunAt(js.RunAt), js.Recurrence); err != nil {
					log.Error("enqueue job", "job_type", js.Type, "error", err)
				}
			}

			var assistantResponse string

			switch result.ResponseType {
			case "inbox_read":
				items, err := inbx.ListOpen()
				if err != nil {
					log.Error("list inbox", "error", err)
					publish(sse.Event{Type: "error", Content: "could not read inbox"})
					return
				}
				var sb strings.Builder
				if len(items) == 0 {
					sb.WriteString("Your inbox is empty.")
				} else {
					fmt.Fprintf(&sb, "You have %d open inbox item(s):\n\n", len(items))
					for i, it := range items {
						fmt.Fprintf(&sb, "%d. %s\n", i+1, it.Question)
					}
				}
				publish(sse.Event{Type: "token", Content: sb.String()})
				publish(sse.Event{Type: "done"})
				assistantResponse = sb.String()

			case "url_fetch":
				publish(sse.Event{Type: "token", Content: "On it — fetching that for you now."})
				publish(sse.Event{Type: "done"})
				assistantResponse = "On it — fetching that for you now."

			case "scheduled", "scheduled_once", "scheduled_recurring":
				msg := buildScheduleConfirmation(result.Jobs)
				publish(sse.Event{Type: "token", Content: msg})
				publish(sse.Event{Type: "done"})
				assistantResponse = msg

			case "job_list":
				pending, err := jobStore.ListPending(ag.ID)
				if err != nil {
					log.Error("list pending jobs", "error", err)
					publish(sse.Event{Type: "error", Content: "could not list scheduled tasks"})
					return
				}
				var sb strings.Builder
				if len(pending) == 0 {
					sb.WriteString("No scheduled tasks.")
				} else {
					fmt.Fprintf(&sb, "You have %d scheduled task(s):\n\n", len(pending))
					for i, j := range pending {
						due := time.Until(j.RunAt).Round(time.Second)
						if j.Recurrence != "" {
							fmt.Fprintf(&sb, "%d. [#%d] %s — due in ~%s (every %s)\n", i+1, j.ID, jobPromptPreview(j), due, j.Recurrence)
						} else {
							fmt.Fprintf(&sb, "%d. [#%d] %s — due in ~%s\n", i+1, j.ID, jobPromptPreview(j), due)
						}
					}
				}
				publish(sse.Event{Type: "token", Content: sb.String()})
				publish(sse.Event{Type: "done"})
				assistantResponse = sb.String()

			case "feature_requests":
				items, err := inbx.ListByCategory("feature_request")
				if err != nil {
					log.Error("list feature requests", "error", err)
					publish(sse.Event{Type: "error", Content: "could not read feature requests"})
					return
				}
				var sb strings.Builder
				if len(items) == 0 {
					sb.WriteString("No open feature requests.")
				} else {
					fmt.Fprintf(&sb, "You have %d open feature request(s):\n\n", len(items))
					for i, it := range items {
						fmt.Fprintf(&sb, "%d. %s\n", i+1, it.Question)
					}
				}
				publish(sse.Event{Type: "token", Content: sb.String()})
				publish(sse.Event{Type: "done"})
				assistantResponse = sb.String()

			case "job_cancel":
				var msg string
				if result.CancelJobID != nil {
					if err := jobStore.Cancel(*result.CancelJobID); err != nil {
						msg = "No pending task found with that ID."
					} else {
						msg = fmt.Sprintf("Canceled task #%d.", *result.CancelJobID)
					}
				} else {
					msg = "Please specify the task ID to cancel."
				}
				publish(sse.Event{Type: "token", Content: msg})
				publish(sse.Event{Type: "done"})
				assistantResponse = msg

			default: // "immediate" or anything unknown
				systemPrompt := buildSystemPrompt(agentDisplayName(ag), ag.Soul, sk, result.RelevantSkillNames)
				msgs := buildMessages(history)

				full, err := cl.Stream(ctx, systemPrompt, msgs, func(token string) error {
					return broker.Publish(ag.ID, sse.Event{Type: "token", Content: token})
				})
				if err != nil {
					log.Error("claude stream", "error", err)
					publish(sse.Event{Type: "error", Content: "stream failed"})
					return
				}
				publish(sse.Event{Type: "done"})
				assistantResponse = full
			}

			// Persist and maybe compact — runs once, sequentially, in the same goroutine.
			// No inner goroutine needed: the user already has their response via SSE.
			if assistantResponse != "" {
				persistAssistant(assistantResponse)
				if compactionThreshold > 0 {
					maybeCompact(ctx, log, chats, cl, ag.ID, agentDisplayName(ag), compactionThreshold)
				}

				// Capability-gap detection: if the response indicates a missing capability,
				// file a feature_request inbox item and notify the user via SSE.
				if detectCapabilityGap(assistantResponse) {
					description := fmt.Sprintf("User asked: %q\n\nSyl responded: %q", req.Text, truncateGapSnippet(assistantResponse))
					if _, err := jobStore.Enqueue(ag.ID, "inbox_write", map[string]string{
						"question": description,
						"category": "feature_request",
					}, time.Now(), ""); err != nil {
						log.Error("enqueue feature request", "error", err)
					} else {
						notice := "It looks like I wasn't able to help with that. I've filed a feature request so we can look into adding that capability."
						publish(sse.Event{Type: "token", Content: notice})
						publish(sse.Event{Type: "done"})
					}
				}
			}
		}()
	}
}

func recentHistory(ctx context.Context, log *slog.Logger, chats chat.Store, agentID int64, limit int) []string {
	msgs, err := chats.Recent(agentID, limit)
	if err != nil {
		log.Error("recent messages", "error", err)
		return nil
	}
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role + ": " + m.Content
	}
	return out
}

// compactionAwareHistory returns the summary (if any) followed by messages since it.
// Falls back to plain recent history when no summary exists.
func compactionAwareHistory(ctx context.Context, log *slog.Logger, chats chat.Store, agentID int64, limit int) []string {
	summary, err := chats.LatestSummary(agentID)
	if err != nil {
		log.Error("latest summary", "error", err)
		return recentHistory(ctx, log, chats, agentID, limit)
	}
	if summary == nil {
		return recentHistory(ctx, log, chats, agentID, limit)
	}
	msgs, err := chats.Since(agentID, summary.ID, limit)
	if err != nil {
		log.Error("messages since summary", "error", err)
		return recentHistory(ctx, log, chats, agentID, limit)
	}
	out := make([]string, 0, len(msgs)+1)
	out = append(out, "summary: "+summary.Content)
	for _, m := range msgs {
		out = append(out, m.Role+": "+m.Content)
	}
	return out
}

// maybeCompact checks if compaction is needed and runs it if so.
// Runs in a goroutine; errors are logged and non-fatal.
func maybeCompact(ctx context.Context, log *slog.Logger, chats chat.Store, cl claude.Client, agentID int64, agentName string, threshold int) {
	summary, err := chats.LatestSummary(agentID)
	if err != nil {
		log.Error("compaction: latest summary", "error", err)
		return
	}
	var afterID int64
	if summary != nil {
		afterID = summary.ID
	}
	count, err := chats.CountSince(agentID, afterID)
	if err != nil {
		log.Error("compaction: count since", "error", err)
		return
	}
	if count < threshold {
		return
	}
	msgs, err := chats.Since(agentID, afterID, count)
	if err != nil {
		log.Error("compaction: fetch messages", "error", err)
		return
	}
	var sb strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&sb, "%s: %s\n", m.Role, m.Content)
	}
	prompt := fmt.Sprintf(compactionUserPromptTmpl, agentName, sb.String())
	summaryText, err := cl.Complete(ctx, compactionSystemPrompt, []claude.Message{{Role: "user", Content: prompt}})
	if err != nil {
		log.Error("compaction: claude call", "error", err)
		return
	}
	if _, err := chats.Add(agentID, "summary", summaryText); err != nil {
		log.Error("compaction: store summary", "error", err)
	} else {
		log.Info("compaction: summary stored", "agent_id", agentID, "msgs_compacted", count)
	}
}

func agentDisplayName(ag *agent.Agent) string {
	if ag.Name.Valid && ag.Name.String != "" {
		return ag.Name.String
	}
	return "Syl"
}

func buildSystemPrompt(name, soul string, sk *skills.Loader, skillNames []string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "You are %s, a personal AI assistant. Always respond as %s.\n", name, name)
	sb.WriteString("Never reveal that you are Claude or that you were built by Anthropic. You are ")
	sb.WriteString(name)
	sb.WriteString(".\n\n")
	if soul != "" {
		sb.WriteString(soul)
		sb.WriteString("\n\n")
	}
	if len(skillNames) > 0 {
		sb.WriteString(sk.GetMany(skillNames))
	}
	return sb.String()
}

func buildMessages(history []string) []claude.Message {
	msgs := make([]claude.Message, 0, len(history))
	for _, h := range history {
		role, content, ok := strings.Cut(h, ": ")
		if !ok {
			continue
		}
		switch role {
		case "user", "assistant":
			msgs = append(msgs, claude.Message{Role: role, Content: content})
		case "summary":
			// Inject summary as a synthetic exchange so Claude has prior context.
			msgs = append(msgs, claude.Message{Role: "user", Content: "[Conversation summary: " + content + "]"})
			msgs = append(msgs, claude.Message{Role: "assistant", Content: "Understood. I have the context from our previous conversation."})
		}
	}
	return msgs
}

func buildScheduleConfirmation(jobs []classifier.JobSpec) string {
	if len(jobs) == 0 {
		return "Got it — scheduled."
	}
	if len(jobs) == 1 {
		j := jobs[0]
		runAt := parseRunAt(j.RunAt)
		delay := time.Until(runAt).Round(time.Second)
		if delay < 0 {
			delay = 0
		}
		if j.Recurrence != "" {
			return fmt.Sprintf("Got it — I'll respond every %s, starting in ~%s.", j.Recurrence, delay)
		}
		return fmt.Sprintf("Got it — I'll respond in ~%s.", delay)
	}
	var sb strings.Builder
	sb.WriteString("Got it — scheduled:\n")
	for i, j := range jobs {
		runAt := parseRunAt(j.RunAt)
		delay := time.Until(runAt).Round(time.Second)
		if delay < 0 {
			delay = 0
		}
		if j.Recurrence != "" {
			fmt.Fprintf(&sb, "%d. every %s, starting in ~%s\n", i+1, j.Recurrence, delay)
		} else {
			fmt.Fprintf(&sb, "%d. in ~%s\n", i+1, delay)
		}
	}
	return sb.String()
}

func jobPromptPreview(j *jobs.Job) string {
	var payload struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(j.Payload, &payload); err != nil || payload.Prompt == "" {
		return j.JobType
	}
	runes := []rune(payload.Prompt)
	if len(runes) > 60 {
		return string(runes[:60]) + "…"
	}
	return payload.Prompt
}

func parseRunAt(s string) time.Time {
	if s == "" {
		return time.Now()
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Now()
	}
	return t
}

type historyItem struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

func handleHistory(chats chat.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID, err := strconv.ParseInt(r.URL.Query().Get("agent_id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid agent_id", http.StatusBadRequest)
			return
		}
		msgs, err := chats.History(agentID, 100_000, 1000)
		if err != nil {
			logger.FromRequest(r).Error("history", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		out := make([]historyItem, 0, len(msgs))
		for _, m := range msgs {
			out = append(out, historyItem{Role: m.Role, Content: m.Content, CreatedAt: m.CreatedAt})
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			logger.FromRequest(r).Error("encode history", "error", err)
		}
	}
}

func handleInboxList(inbx inbox.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := inbx.List()
		if err != nil {
			logger.FromRequest(r).Error("list inbox", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(items); err != nil {
			logger.FromRequest(r).Error("encode inbox list", "error", err)
		}
	}
}

func handleInboxAnswer(inbx inbox.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		var body struct {
			Answer string `json:"answer"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := inbx.Answer(id, body.Answer); err != nil {
			logger.FromRequest(r).Error("answer inbox", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
}

func handleGetSoul(agents agent.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		ag, err := agents.Get(id)
		if err != nil {
			logger.FromRequest(r).Error("get agent", "error", err)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"soul": ag.Soul}); err != nil {
			logger.FromRequest(r).Error("encode soul", "error", err)
		}
	}
}

func handlePutSoul(agents agent.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		var body struct {
			Soul string `json:"soul"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := agents.UpdateSoul(id, body.Soul); err != nil {
			logger.FromRequest(r).Error("update soul", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// capabilityGapPhrases are patterns that indicate Claude lacks a capability the user asked for.
var capabilityGapPhrases = []string{
	"i don't have access",
	"i do not have access",
	"i can't access",
	"i cannot access",
	"i'm unable to",
	"i am unable to",
	"i don't have the ability",
	"i do not have the ability",
	"i lack the ability",
	"i don't have real-time",
	"i don't have information about your",
	"i can't retrieve",
	"i cannot retrieve",
	"i can't look up",
	"i cannot look up",
	"i can't check",
	"i cannot check",
}

// truncateGapSnippet returns the first 200 runes of a response for use in gap descriptions.
func truncateGapSnippet(s string) string {
	runes := []rune(s)
	if len(runes) <= 200 {
		return s
	}
	return string(runes[:200]) + "…"
}

// detectCapabilityGap returns true if the response contains a refusal pattern
// suggesting the assistant lacks a capability the user asked for.
func detectCapabilityGap(response string) bool {
	lower := strings.ToLower(response)
	for _, phrase := range capabilityGapPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// compactionSystemPrompt and compactionUserPromptTmpl control the summarization
// call made by maybeCompact. Kept as constants so they're easy to find and tune.
const (
	compactionSystemPrompt   = "You are a conversation summarizer. Be concise and factual."
	compactionUserPromptTmpl = "Summarize the following conversation compactly. Preserve key facts, preferences, decisions, and context that would be important for %s's future interactions with the user.\n\n%s"
)

// jobProcessor implements jobs.Processor for the server.
type jobProcessor struct {
	claude              claude.Client
	broker              *sse.Broker
	agents              agent.Store
	chats               chat.Store
	inboxItems          inbox.Store
	skills              *skills.Loader
	jobStore            jobs.Store
	clf                 classifier.Classifier
	logger              *slog.Logger
	compactionThreshold int
	historyLimit        int
	fetchDenylist       []string
	fetchContentLimit   int
}

func (p *jobProcessor) Process(job *jobs.Job) error {
	p.logger.Info("processing job", "job_id", job.ID, "job_type", job.JobType)
	switch job.JobType {
	case "send_message":
		return p.processSendMessage(job)
	case "inbox_write":
		return p.processInboxWrite(job)
	case "skill_write":
		return p.processSkillWrite(job)
	case "soul_update":
		return p.processSoulUpdate(job)
	case "url_fetch":
		return p.processURLFetch(job)
	default:
		p.logger.Warn("unknown job type — skipping", "job_id", job.ID, "job_type", job.JobType)
		return nil
	}
}

func (p *jobProcessor) processSendMessage(job *jobs.Job) error {
	ctx := context.Background()

	var payload struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}

	ag, err := p.agents.Get(job.AgentID)
	if err != nil {
		return fmt.Errorf("get agent: %w", err)
	}

	history := compactionAwareHistory(ctx, p.logger, p.chats, job.AgentID, p.historyLimit)

	// Classify the prompt — it may itself contain scheduling intent.
	result, err := p.clf.Classify(ctx, ag.Soul, history, p.skills.Names(), payload.Prompt)
	if err != nil {
		p.logger.Error("classify job prompt", "job_id", job.ID, "error", err)
		result = &classifier.Result{ResponseType: "immediate"}
	}

	// Apply soul update if present.
	if result.SoulUpdate != nil && *result.SoulUpdate != "" {
		if err := p.agents.UpdateSoul(ag.ID, *result.SoulUpdate); err != nil {
			p.logger.Error("update soul", "error", err)
		}
	}

	// Enqueue any new jobs the classifier found.
	for _, js := range result.Jobs {
		if _, err := p.jobStore.Enqueue(ag.ID, js.Type, js.Payload, parseRunAt(js.RunAt), js.Recurrence); err != nil {
			p.logger.Error("enqueue job from prompt", "job_type", js.Type, "error", err)
		}
	}

	switch result.ResponseType {
	case "scheduled", "scheduled_once", "scheduled_recurring":
		// Jobs already enqueued above; nothing to stream.

	default: // "immediate" or anything else → call Claude
		systemPrompt := buildSystemPrompt(agentDisplayName(ag), ag.Soul, p.skills, result.RelevantSkillNames)
		msgs := buildMessages(history)
		msgs = append(msgs, claude.Message{Role: "user", Content: payload.Prompt})

		response, err := p.claude.Complete(ctx, systemPrompt, msgs)
		if err != nil {
			return fmt.Errorf("claude complete: %w", err)
		}

		if _, err := p.chats.Add(job.AgentID, "assistant", response); err != nil {
			p.logger.Error("persist assistant message", "error", err)
		} else if p.compactionThreshold > 0 {
			maybeCompact(ctx, p.logger, p.chats, p.claude, job.AgentID, agentDisplayName(ag), p.compactionThreshold)
		}
		if err := p.broker.Publish(job.AgentID, sse.Event{Type: "token", Content: response}); err != nil {
			p.logger.Error("publish token", "error", err)
		}
		if err := p.broker.Publish(job.AgentID, sse.Event{Type: "done"}); err != nil {
			p.logger.Error("publish done", "error", err)
		}
	}

	// Re-enqueue for recurring jobs (only for send_message).
	if job.Recurrence != "" {
		dur, err := time.ParseDuration(job.Recurrence)
		if err == nil {
			nextRunAt := job.RunAt.Add(dur)
			if _, err := p.jobStore.Enqueue(job.AgentID, job.JobType, json.RawMessage(job.Payload), nextRunAt, job.Recurrence); err != nil {
				p.logger.Error("re-enqueue recurring job", "error", err)
			}
		}
	}

	return nil
}

func (p *jobProcessor) processInboxWrite(job *jobs.Job) error {
	var payload struct {
		Question string `json:"question"`
		Category string `json:"category"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	if _, err := p.inboxItems.Add(payload.Question, payload.Category); err != nil {
		return fmt.Errorf("add inbox item: %w", err)
	}
	msg := fmt.Sprintf("I've added a question to your inbox: %q", payload.Question)
	_ = p.broker.Publish(job.AgentID, sse.Event{Type: "token", Content: msg})
	_ = p.broker.Publish(job.AgentID, sse.Event{Type: "done"})
	return nil
}

func (p *jobProcessor) processSkillWrite(job *jobs.Job) error {
	var payload struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	if err := p.skills.Write(payload.Name, payload.Content); err != nil {
		return fmt.Errorf("write skill: %w", err)
	}
	msg := fmt.Sprintf("Skill %q has been created.", payload.Name)
	_ = p.broker.Publish(job.AgentID, sse.Event{Type: "token", Content: msg})
	_ = p.broker.Publish(job.AgentID, sse.Event{Type: "done"})
	return nil
}

func (p *jobProcessor) processSoulUpdate(job *jobs.Job) error {
	var payload struct {
		Soul string `json:"soul"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	return p.agents.UpdateSoul(job.AgentID, payload.Soul)
}
