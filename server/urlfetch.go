package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/sethgrid/syl/internal/claude"
	"github.com/sethgrid/syl/internal/jobs"
	"github.com/sethgrid/syl/internal/sse"
)

const fetchUserAgent = "Syl/1.0 (personal assistant; +https://github.com/sethgrid/syl)"

func (p *jobProcessor) processURLFetch(job *jobs.Job) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var payload struct {
		URL      string `json:"url"`
		Question string `json:"question"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	if payload.URL == "" {
		return fmt.Errorf("url_fetch: missing url in payload")
	}

	// Deny list check.
	if err := checkDenylist(payload.URL, p.fetchDenylist); err != nil {
		msg := fmt.Sprintf("I can't fetch that URL: %s", err)
		_ = p.broker.Publish(job.AgentID, sse.Event{Type: "token", Content: msg})
		_ = p.broker.Publish(job.AgentID, sse.Event{Type: "done"})
		return nil
	}

	p.logger.Info("url_fetch: fetching", "url", payload.URL, "job_id", job.ID)

	content, err := fetchAndExtract(ctx, payload.URL, p.fetchContentLimit)
	if err != nil {
		msg := fmt.Sprintf("I wasn't able to fetch that URL: %s", err)
		_ = p.broker.Publish(job.AgentID, sse.Event{Type: "token", Content: msg})
		_ = p.broker.Publish(job.AgentID, sse.Event{Type: "done"})
		return nil
	}

	ag, err := p.agents.Get(job.AgentID)
	if err != nil {
		return fmt.Errorf("get agent: %w", err)
	}

	systemPrompt := buildSystemPrompt(agentDisplayName(ag), ag.Soul, p.skills, nil)
	msgs := []claude.Message{
		{
			Role: "user",
			Content: fmt.Sprintf(
				"I fetched the following content from %s:\n\n---\n%s\n---\n\n%s",
				payload.URL, content, payload.Question,
			),
		},
	}

	response, err := p.claude.Complete(ctx, systemPrompt, msgs)
	if err != nil {
		return fmt.Errorf("claude complete: %w", err)
	}

	if _, err := p.chats.Add(job.AgentID, "assistant", response); err != nil {
		p.logger.Error("persist assistant message", "error", err)
	}
	_ = p.broker.Publish(job.AgentID, sse.Event{Type: "token", Content: response})
	_ = p.broker.Publish(job.AgentID, sse.Event{Type: "done"})
	return nil
}

// checkDenylist returns an error if rawURL's host matches any entry in the deny list.
func checkDenylist(rawURL string, denylist []string) error {
	if len(denylist) == 0 {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL")
	}
	host := strings.ToLower(u.Hostname())
	for _, denied := range denylist {
		denied = strings.ToLower(strings.TrimSpace(denied))
		if host == denied || strings.HasSuffix(host, "."+denied) {
			return fmt.Errorf("domain %q is on the deny list", host)
		}
	}
	return nil
}

// fetchAndExtract fetches a URL and returns plain text extracted from the HTML,
// truncated to contentLimit runes.
func fetchAndExtract(ctx context.Context, rawURL string, contentLimit int) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", fetchUserAgent)
	req.Header.Set("Accept", "text/html,text/plain;q=0.9,*/*;q=0.8")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d from server", resp.StatusCode)
	}

	// Read at most 1MB to avoid memory issues with large pages.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	var text string
	if strings.Contains(contentType, "text/html") || contentType == "" {
		text = extractText(string(body))
	} else {
		text = string(body)
	}

	// Truncate to contentLimit runes, rune-safe.
	runes := []rune(strings.TrimSpace(text))
	if len(runes) > contentLimit {
		runes = runes[:contentLimit]
		return string(runes) + "\n[content truncated]", nil
	}
	return string(runes), nil
}

// extractText walks an HTML token stream and returns visible text content,
// skipping script, style, and other non-visible elements.
func extractText(htmlContent string) string {
	var sb strings.Builder
	tokenizer := html.NewTokenizer(strings.NewReader(htmlContent))
	skip := 0 // depth counter for skipped elements

	skipTags := map[string]bool{
		"script": true, "style": true, "noscript": true,
		"head": true, "nav": true, "footer": true,
	}
	blockTags := map[string]bool{
		"p": true, "div": true, "br": true, "li": true,
		"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
		"tr": true, "td": true, "th": true, "blockquote": true,
	}

	for {
		tt := tokenizer.Next()
		switch tt {
		case html.ErrorToken:
			return sb.String()

		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := tokenizer.TagName()
			tag := string(name)
			if skipTags[tag] {
				skip++
			}
			if blockTags[tag] && sb.Len() > 0 {
				last := sb.String()
				if !strings.HasSuffix(last, "\n") {
					sb.WriteByte('\n')
				}
			}

		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			tag := string(name)
			if skipTags[tag] && skip > 0 {
				skip--
			}

		case html.TextToken:
			if skip > 0 {
				continue
			}
			text := strings.TrimSpace(string(tokenizer.Text()))
			if text != "" {
				sb.WriteString(text)
				sb.WriteByte(' ')
			}
		}
	}
}
