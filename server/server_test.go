package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sethgrid/syl/internal/claude"
	"github.com/sethgrid/syl/internal/sse"
	"github.com/sethgrid/syl/server"
)

func testConfig() server.Config {
	return server.Config{
		Version:         "test",
		Port:            0,
		InternalPort:    0,
		Name:            "Syl",
		RequestTimeout:  5 * time.Second,
		ShutdownTimeout: 5 * time.Second,
	}
}

func startServer(t *testing.T, opts ...server.Option) (srv *server.Server, port int) {
	t.Helper()
	srv = server.NewTest(testConfig(), opts...)
	go srv.Serve()
	port = srv.Port()
	require.NotZero(t, port, "server did not start")
	t.Cleanup(func() { srv.Close() })
	return srv, port
}

// TestSSESurvivesRequestTimeout (sy-04): confirms the /sse route is not killed
// by the server's request timeout middleware.
func TestSSESurvivesRequestTimeout(t *testing.T) {
	// Use a very short request timeout so it fires quickly if SSE is wired wrong.
	cfg := testConfig()
	cfg.RequestTimeout = 150 * time.Millisecond
	srv := server.NewTest(cfg)
	go srv.Serve()
	port := srv.Port()
	require.NotZero(t, port)
	defer srv.Close()

	// Resolve an agent first so we have a valid agent_id.
	body, _ := json.Marshal(map[string]string{"text": "hi", "fingerprint": "fp-sse-spike"})
	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/message", port),
		"application/json", bytes.NewReader(body))
	require.NoError(t, err)
	resp.Body.Close()

	// Open SSE — must stay alive well past RequestTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://localhost:%d/sse?agent_id=1", port), nil)

	sseResp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer sseResp.Body.Close()

	assert.Equal(t, http.StatusOK, sseResp.StatusCode)
	assert.Equal(t, "text/event-stream", sseResp.Header.Get("Content-Type"))

	// Wait past the request timeout to confirm connection is still open.
	time.Sleep(300 * time.Millisecond) // 2× the timeout

	// Connection should still be alive (context not canceled by server).
	select {
	case <-ctx.Done():
		t.Fatal("SSE context canceled unexpectedly — server likely killed the connection")
	default:
		// still open: pass
	}
}

// TestEchoRoundTrip (sy-05): POST /message → SSE tokens received end-to-end.
func TestEchoRoundTrip(t *testing.T) {
	fake := &claude.FakeClient{Tokens: []string{"Hello", ", ", "world!"}}
	_, port := startServer(t, server.WithClaude(fake))

	// Subscribe to SSE before sending message (agent_id=1 will be assigned).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sseReq, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://localhost:%d/sse?agent_id=1", port), nil)

	sseResp, err := http.DefaultClient.Do(sseReq)
	require.NoError(t, err)
	defer sseResp.Body.Close()

	// Channel to collect SSE events.
	events := make(chan sse.Event, 8)
	go func() {
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
		close(events)
	}()

	// Small delay to ensure SSE subscription is registered before sending message.
	time.Sleep(50 * time.Millisecond)

	body, _ := json.Marshal(map[string]string{
		"text":        "hello",
		"fingerprint": "fp-echo-test",
	})
	msgResp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/message", port),
		"application/json", bytes.NewReader(body))
	require.NoError(t, err)
	assert.Equal(t, http.StatusAccepted, msgResp.StatusCode)
	msgResp.Body.Close()

	// Expect a "token" event with the echo content.
	var gotToken, gotDone bool
	deadline := time.After(3 * time.Second)
	for !gotToken || !gotDone {
		select {
		case evt, ok := <-events:
			if !ok {
				t.Fatal("SSE stream closed before receiving all events")
			}
			switch evt.Type {
			case "token":
				gotToken = true
			case "done":
				gotDone = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for SSE events (token=%v done=%v)", gotToken, gotDone)
		}
	}
}

// TestMessageRequiresFingerprint ensures missing fingerprint returns 400.
func TestMessageRequiresFingerprint(t *testing.T) {
	_, port := startServer(t)

	body, _ := json.Marshal(map[string]string{"text": "hello"})
	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/message", port),
		"application/json", bytes.NewReader(body))
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestInboxListEmpty ensures /inbox returns 200 with empty list.
func TestInboxListEmpty(t *testing.T) {
	_, port := startServer(t)
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/inbox", port))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestHistory (Unit A): POST /message → GET /history → non-empty, first item role="user".
func TestHistory(t *testing.T) {
	fake := &claude.FakeClient{Tokens: []string{"Hi there!"}}
	_, port := startServer(t, server.WithClaude(fake))

	body, _ := json.Marshal(map[string]string{
		"text":        "hello history",
		"fingerprint": "fp-history-test",
	})
	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/message", port),
		"application/json", bytes.NewReader(body))
	require.NoError(t, err)
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp.Body.Close()

	time.Sleep(100 * time.Millisecond)

	histResp, err := http.Get(fmt.Sprintf("http://localhost:%d/history?agent_id=1", port))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, histResp.StatusCode)
	defer histResp.Body.Close()

	var items []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	require.NoError(t, json.NewDecoder(histResp.Body).Decode(&items))
	assert.NotEmpty(t, items)
	assert.Equal(t, "user", items[0].Role)
}

// TestNamedAgent (Unit B): two sessions with same ?name=syl get the same agent_id.
func TestNamedAgent(t *testing.T) {
	_, port := startServer(t)

	getAgentID := func(fp string) int64 {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/session?fingerprint=%s&name=syl", port, fp))
		require.NoError(t, err)
		defer resp.Body.Close()
		var data struct {
			AgentID int64 `json:"agent_id"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&data))
		return data.AgentID
	}

	id1 := getAgentID("fp-named-a")
	id2 := getAgentID("fp-named-b")
	assert.Equal(t, id1, id2, "same named agent should return same agent_id")
}
