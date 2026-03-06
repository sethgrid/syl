package server_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sethgrid/syl/internal/claude"
	"github.com/sethgrid/syl/server"
)

// postChat sends a POST /chat request and returns the response.
// It asserts that no transport error occurred; status code checking is left to callers.
func postChat(t *testing.T, port int, fingerprint, text string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"fingerprint": fingerprint,
		"text":        text,
	})
	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/chat", port),
		"application/json",
		bytes.NewReader(body),
	)
	require.NoError(t, err)
	return resp
}

// TestChatSmoke verifies that POST /chat returns 200 with the expected JSON body.
// Uses FakeClient so it always runs and gates CI.
func TestChatSmoke(t *testing.T) {
	fake := &claude.FakeClient{Tokens: []string{"pong"}}
	_, port := startServer(t, server.WithClaude(fake))

	resp := postChat(t, port, "fp-smoke", "ping")
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	var result map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	assert.Equal(t, "pong", result["response"])
}

// TestChatRequiresFingerprint verifies that a missing fingerprint returns 400.
func TestChatRequiresFingerprint(t *testing.T) {
	_, port := startServer(t)

	body, _ := json.Marshal(map[string]string{"text": "hello"})
	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/chat", port),
		"application/json",
		bytes.NewReader(body),
	)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestChatClaudeError verifies that a Claude failure returns 500.
func TestChatClaudeError(t *testing.T) {
	fake := &claude.FakeClient{Err: errors.New("upstream timeout")}
	_, port := startServer(t, server.WithClaude(fake))

	resp := postChat(t, port, "fp-err", "ping")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

// TestChatIntegration calls the real Claude API.
// Skipped unless SYL_ANTHROPIC_API_KEY is set.
func TestChatIntegration(t *testing.T) {
	apiKey := os.Getenv("SYL_ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("SYL_ANTHROPIC_API_KEY not set — skipping integration test")
	}

	cfg := testConfig()
	cfg.RequestTimeout = 30 * time.Second
	srv := server.NewTest(cfg, server.WithClaude(claude.New(apiKey)))
	go srv.Serve()
	port := srv.Port()
	require.NotZero(t, port, "server did not start")
	defer srv.Close()

	resp := postChat(t, port, "fp-integration", "Reply with exactly the word: pong")
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var result map[string]string
	require.NoError(t, json.Unmarshal(raw, &result))
	assert.True(t, strings.Contains(strings.ToLower(result["response"]), "pong"),
		"expected response to contain 'pong', got: %s", result["response"])
}
