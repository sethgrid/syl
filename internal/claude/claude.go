package claude

import (
	"context"
	"fmt"
	"strings"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Message is a single turn in a conversation.
type Message struct {
	Role    string // "user" or "assistant"
	Content string
}

// Client is the interface for Claude API interactions.
type Client interface {
	// Stream calls Claude and invokes onToken for each text token.
	// Accumulates and returns the full response when complete.
	Stream(ctx context.Context, systemPrompt string, messages []Message, onToken func(string) error) (string, error)

	// Complete calls Claude and returns the full response without streaming.
	Complete(ctx context.Context, systemPrompt string, messages []Message) (string, error)
}

// RealClient wraps the Anthropic Go SDK.
type RealClient struct {
	inner *anthropic.Client
	model string
}

func New(apiKey string) *RealClient {
	c := anthropic.NewClient(option.WithAPIKey(apiKey))
	return &RealClient{
		inner: c,
		model: "claude-haiku-4-5-20251001",
	}
}

// WithModel returns a copy of the client using the specified model.
func (c *RealClient) WithModel(model string) *RealClient {
	cp := *c
	cp.model = model
	return &cp
}

func (c *RealClient) Stream(ctx context.Context, systemPrompt string, messages []Message, onToken func(string) error) (string, error) {
	params := c.buildParams(systemPrompt, messages)
	stream := c.inner.Messages.NewStreaming(ctx, params)
	defer stream.Close()

	var sb strings.Builder
	for stream.Next() {
		evt := stream.Current()
		if delta, ok := evt.Delta.(anthropic.ContentBlockDeltaEventDelta); ok {
			if delta.Text != "" {
				if err := onToken(delta.Text); err != nil {
					return sb.String(), fmt.Errorf("onToken: %w", err)
				}
				sb.WriteString(delta.Text)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return sb.String(), fmt.Errorf("stream: %w", err)
	}
	return sb.String(), nil
}

func (c *RealClient) Complete(ctx context.Context, systemPrompt string, messages []Message) (string, error) {
	params := c.buildParams(systemPrompt, messages)
	msg, err := c.inner.Messages.New(ctx, params)
	if err != nil {
		return "", fmt.Errorf("complete: %w", err)
	}
	if len(msg.Content) == 0 {
		return "", nil
	}
	// First content block is the text response.
	if block, ok := msg.Content[0].AsUnion().(anthropic.TextBlock); ok {
		return block.Text, nil
	}
	return "", nil
}

func (c *RealClient) buildParams(systemPrompt string, messages []Message) anthropic.MessageNewParams {
	params := anthropic.MessageNewParams{
		Model:     anthropic.F(anthropic.Model(c.model)),
		MaxTokens: anthropic.Int(4096),
	}
	if systemPrompt != "" {
		params.System = anthropic.F([]anthropic.TextBlockParam{
			anthropic.NewTextBlock(systemPrompt),
		})
	}
	var msgParams []anthropic.MessageParam
	for _, m := range messages {
		block := anthropic.NewTextBlock(m.Content)
		switch m.Role {
		case "user":
			msgParams = append(msgParams, anthropic.NewUserMessage(block))
		case "assistant":
			msgParams = append(msgParams, anthropic.NewAssistantMessage(block))
		}
	}
	params.Messages = anthropic.F(msgParams)
	return params
}

// FakeClient is an in-memory implementation for testing.
type FakeClient struct {
	Tokens []string
	Err    error
}

func (f *FakeClient) Stream(_ context.Context, _ string, _ []Message, onToken func(string) error) (string, error) {
	if f.Err != nil {
		return "", f.Err
	}
	var sb strings.Builder
	for _, t := range f.Tokens {
		if err := onToken(t); err != nil {
			return sb.String(), err
		}
		sb.WriteString(t)
	}
	return sb.String(), nil
}

func (f *FakeClient) Complete(_ context.Context, _ string, _ []Message) (string, error) {
	if f.Err != nil {
		return "", f.Err
	}
	return strings.Join(f.Tokens, ""), nil
}
