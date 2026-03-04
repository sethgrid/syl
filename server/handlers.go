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

	"github.com/sethgrid/syl/internal/agent"
	"github.com/sethgrid/syl/internal/chat"
	"github.com/sethgrid/syl/internal/classifier"
	"github.com/sethgrid/syl/internal/claude"
	"github.com/sethgrid/syl/internal/inbox"
	"github.com/sethgrid/syl/internal/jobs"
	"github.com/sethgrid/syl/internal/skills"
	"github.com/sethgrid/syl/internal/sse"
	"github.com/sethgrid/syl/logger"
)

func handleIndex() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "web/index.html")
	}
}

func handleHealthcheck() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "OK")
	}
}

func handleStatus(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"version": version,
			"status":  "ok",
		})
	}
}

func handleSSE(broker *sse.Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID, err := strconv.ParseInt(r.URL.Query().Get("agent_id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid agent_id", http.StatusBadRequest)
			return
		}
		broker.Subscribe(agentID, w, r)
	}
}

type messageReq struct {
	Text        string `json:"text"`
	AgentName   string `json:"agent_name"`
	Fingerprint string `json:"fingerprint"`
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
		go func() {
			ctx := r.Context()

			// 4. Pre-classify.
			history := recentHistory(ctx, log, chats, ag.ID, 20)
			result, err := clf.Classify(ctx, ag.Soul, history, sk.Names(), req.Text)
			if err != nil {
				log.Error("classify", "error", err)
				broker.Publish(ag.ID, sse.Event{Type: "error", Content: "classification failed"})
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
				if _, err := jobStore.Enqueue(ag.ID, js.Type, js.Payload, parseRunAt(js.RunAt)); err != nil {
					log.Error("enqueue job", "job_type", js.Type, "error", err)
				}
			}

			switch result.ResponseType {
			case "inbox_read":
				// Fetch open inbox items and stream them as a formatted response.
				items, err := inbx.ListOpen()
				if err != nil {
					log.Error("list inbox", "error", err)
					broker.Publish(ag.ID, sse.Event{Type: "error", Content: "could not read inbox"})
					return
				}
				var sb strings.Builder
				if len(items) == 0 {
					sb.WriteString("Your inbox is empty.")
				} else {
					sb.WriteString(fmt.Sprintf("You have %d open inbox item(s):\n\n", len(items)))
					for i, it := range items {
						sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, it.Question))
					}
				}
				broker.Publish(ag.ID, sse.Event{Type: "token", Content: sb.String()})
				broker.Publish(ag.ID, sse.Event{Type: "done"})
				chats.Add(ag.ID, "assistant", sb.String())

			case "scheduled":
				// Jobs already enqueued above. Nothing to stream now.
				broker.Publish(ag.ID, sse.Event{Type: "done"})

			default: // "immediate" or anything unknown
				systemPrompt := buildSystemPrompt(ag.Soul, sk, result.RelevantSkillNames)
				msgs := buildMessages(history)

				full, err := cl.Stream(ctx, systemPrompt, msgs, func(token string) error {
					return broker.Publish(ag.ID, sse.Event{Type: "token", Content: token})
				})
				if err != nil {
					log.Error("claude stream", "error", err)
					broker.Publish(ag.ID, sse.Event{Type: "error", Content: "stream failed"})
					return
				}
				broker.Publish(ag.ID, sse.Event{Type: "done"})
				if full != "" {
					chats.Add(ag.ID, "assistant", full)
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

func buildSystemPrompt(soul string, sk *skills.Loader, skillNames []string) string {
	var sb strings.Builder
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
		if role == "user" || role == "assistant" {
			msgs = append(msgs, claude.Message{Role: role, Content: content})
		}
	}
	return msgs
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

func handleInboxList(inbx inbox.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := inbx.List()
		if err != nil {
			logger.FromRequest(r).Error("list inbox", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(items)
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
		w.WriteHeader(http.StatusOK)
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
		json.NewEncoder(w).Encode(map[string]string{"soul": ag.Soul})
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

// jobProcessor implements jobs.Processor for the server.
type jobProcessor struct {
	claude     claude.Client
	broker     *sse.Broker
	agents     agent.Store
	chats      chat.Store
	inboxItems inbox.Store
	logger     *slog.Logger
}

func (p *jobProcessor) Process(job *jobs.Job) error {
	// TODO (Epic 4): implement soul_update, send_message, inbox_write job types
	p.logger.Info("processing job", "job_id", job.ID, "job_type", job.JobType)
	return fmt.Errorf("unknown job type: %s", job.JobType)
}
