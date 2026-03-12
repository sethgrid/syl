package server_test

// TestMessageIntegration exercises the full /message → SSE streaming pipeline
// with real Claude and real SQLite.
// Skipped unless SYL_ANTHROPIC_API_KEY is set.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sethgrid/syl/internal/agent"
	"github.com/sethgrid/syl/internal/chat"
	"github.com/sethgrid/syl/internal/classifier"
	"github.com/sethgrid/syl/internal/claude"
	"github.com/sethgrid/syl/internal/db"
	"github.com/sethgrid/syl/internal/sse"
	"github.com/sethgrid/syl/server"
)

func TestMessageIntegration(t *testing.T) {
	apiKey := os.Getenv("SYL_ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("SYL_ANTHROPIC_API_KEY not set — skipping integration test")
	}

	// Real SQLite DB in a temp file.
	f, err := os.CreateTemp("", "syl-e2e-*.db")
	require.NoError(t, err)
	f.Close()
	t.Cleanup(func() {
		os.Remove(f.Name())
		os.Remove(f.Name() + "-wal")
		os.Remove(f.Name() + "-shm")
	})

	sqlDB, err := db.Open(f.Name())
	require.NoError(t, err)
	defer sqlDB.Close()

	claudeClient := claude.New(apiKey)
	clf := classifier.NewClaudeClassifier(claudeClient, time.Now)

	cfg := testConfig()
	cfg.RequestTimeout = 30 * time.Second
	cfg.CompactionThreshold = 0 // disable compaction for this test

	srv := server.NewTest(cfg,
		server.WithAgents(agent.NewSQLiteStore(sqlDB)),
		server.WithChats(chat.NewSQLiteStore(sqlDB)),
		server.WithClaude(claudeClient),
		server.WithClassifier(clf),
	)
	go srv.Serve()
	port := srv.Port()
	require.NotZero(t, port, "server did not start")
	defer srv.Close()

	// Resolve agent to get a stable agent_id.
	sessResp, err := http.Get(fmt.Sprintf("http://localhost:%d/session?fingerprint=fp-e2e", port))
	require.NoError(t, err)
	defer sessResp.Body.Close()
	var sessData struct {
		AgentID int64 `json:"agent_id"`
	}
	require.NoError(t, json.NewDecoder(sessResp.Body).Decode(&sessData))
	agentID := sessData.AgentID
	require.NotZero(t, agentID)

	// Open SSE connection.
	sseURL := fmt.Sprintf("http://localhost:%d/sse?agent_id=%d", port, agentID)
	sseReq, err := http.NewRequest(http.MethodGet, sseURL, nil)
	require.NoError(t, err)

	sseResp, err := http.DefaultClient.Do(sseReq)
	require.NoError(t, err)
	defer sseResp.Body.Close()
	assert.Equal(t, http.StatusOK, sseResp.StatusCode)

	events := make(chan sse.Event, 32)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(sseResp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var evt sse.Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt); err == nil {
				events <- evt
			}
		}
	}()

	// Small delay so SSE subscription is registered.
	time.Sleep(100 * time.Millisecond)

	// POST a message.
	body, _ := json.Marshal(map[string]string{
		"text":        "Reply with exactly the word: pong",
		"fingerprint": "fp-e2e",
	})
	msgResp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/message", port),
		"application/json", bytes.NewReader(body))
	require.NoError(t, err)
	assert.Equal(t, http.StatusAccepted, msgResp.StatusCode)
	msgResp.Body.Close()

	// Collect SSE events until "done" (or timeout).
	var tokens []string
	var gotDone bool
	deadline := time.After(30 * time.Second)
	for !gotDone {
		select {
		case evt, ok := <-events:
			if !ok {
				t.Fatal("SSE stream closed before receiving done event")
			}
			switch evt.Type {
			case "token":
				tokens = append(tokens, evt.Content)
			case "done":
				gotDone = true
			case "error":
				t.Fatalf("SSE error event: %s", evt.Content)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for SSE done (tokens so far: %v)", tokens)
		}
	}

	full := strings.Join(tokens, "")
	assert.True(t, strings.Contains(strings.ToLower(full), "pong"),
		"expected response to contain 'pong', got: %s", full)

	// Verify /history reflects both messages.
	histResp, err := http.Get(fmt.Sprintf("http://localhost:%d/history?agent_id=%d", port, agentID))
	require.NoError(t, err)
	defer histResp.Body.Close()

	var histItems []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	require.NoError(t, json.NewDecoder(histResp.Body).Decode(&histItems))
	require.GreaterOrEqual(t, len(histItems), 2, "expected at least user + assistant messages in history")

	roles := make([]string, len(histItems))
	for i, it := range histItems {
		roles[i] = it.Role
	}
	assert.Contains(t, roles, "user")
	assert.Contains(t, roles, "assistant")
}
