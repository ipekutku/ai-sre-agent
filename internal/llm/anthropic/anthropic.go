// Package anthropic implements llm.Client with the Anthropic Messages API.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/ipekutku/ai-sre-agent/internal/llm"
)

// DefaultModel is the model used when Config.Model is empty.
const DefaultModel = "claude-sonnet-5-5"

// Config configures a Client.
type Config struct {
	// APIKey authenticates requests. If empty, the SDK's default credential
	// resolution applies (ANTHROPIC_API_KEY, then other configured sources).
	APIKey string
	// Model is the model ID. Default DefaultModel.
	Model string
	// MaxTokens bounds each response. Default 16000.
	MaxTokens int64
	// Effort is the output effort level: low, medium, high, xhigh, or max.
	// Default medium, the recommended starting point for multi-step tool use.
	Effort string
	// RequestTimeout bounds each HTTP attempt. Default 2m.
	RequestTimeout time.Duration
	// MaxRetries is the number of SDK retries on 408/409/429/5xx and
	// connection errors. Default 2. Set to -1 to disable retries.
	MaxRetries int
	// BaseURL overrides the API endpoint (tests only).
	BaseURL string
	// HTTPClient overrides the HTTP client (tests only).
	HTTPClient *http.Client
}

var efforts = map[string]sdk.OutputConfigEffort{
	"low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "max",
}

// Client is an llm.Client backed by the Anthropic Messages API.
type Client struct {
	sdk       sdk.Client
	model     string
	maxTokens int64
	effort    sdk.OutputConfigEffort
}

// New returns a Client for cfg.
func New(cfg Config) (*Client, error) {
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 16000
	}
	if cfg.Effort == "" {
		cfg.Effort = "medium"
	}
	effort, ok := efforts[cfg.Effort]
	if !ok {
		return nil, fmt.Errorf("unknown effort %q (want low, medium, high, xhigh, or max)", cfg.Effort)
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 2 * time.Minute
	}
	switch {
	case cfg.MaxRetries < 0:
		cfg.MaxRetries = 0
	case cfg.MaxRetries == 0:
		cfg.MaxRetries = 2
	}

	opts := []option.RequestOption{
		option.WithRequestTimeout(cfg.RequestTimeout),
		option.WithMaxRetries(cfg.MaxRetries),
	}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	return &Client{sdk: sdk.NewClient(opts...), model: cfg.Model, maxTokens: cfg.MaxTokens, effort: effort}, nil
}

// Model returns the configured model ID.
func (c *Client) Model() string { return c.model }

// Check verifies credentials and the model ID with a model lookup, which
// consumes no tokens. Use it to fail fast before a long setup.
func (c *Client) Check(ctx context.Context) error {
	if _, err := c.sdk.Models.Get(ctx, c.model, sdk.ModelGetParams{}); err != nil {
		var apiErr *sdk.Error
		var netErr *url.Error
		if !errors.As(err, &apiErr) && !errors.As(err, &netErr) &&
			!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			// Errors raised before any HTTP request (e.g. no credentials found)
			// are configuration problems, not network failures.
			return &llm.Error{Kind: llm.ErrAuth, Message: err.Error(), Err: err}
		}
		return classify(err)
	}
	return nil
}

// Generate sends one Messages API request.
func (c *Client) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	messages, err := toParams(req.Messages)
	if err != nil {
		return llm.Response{}, &llm.Error{Kind: llm.ErrInvalidRequest, Message: err.Error(), Err: err}
	}
	tools, err := toToolParams(req.Tools)
	if err != nil {
		return llm.Response{}, &llm.Error{Kind: llm.ErrInvalidRequest, Message: err.Error(), Err: err}
	}

	params := sdk.MessageNewParams{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		Messages:  messages,
		Tools:     tools,
		// Thinking is left unset: current models run adaptive thinking by default.
		OutputConfig: sdk.OutputConfigParam{Effort: c.effort},
		// System and tools are stable for a conversation, so cache the prefix.
		CacheControl: sdk.NewCacheControlEphemeralParam(),
	}
	if req.System != "" {
		params.System = []sdk.TextBlockParam{{Text: req.System}}
	}

	start := time.Now()
	msg, err := c.sdk.Messages.New(ctx, params)
	latency := time.Since(start)
	if err != nil {
		return llm.Response{}, classify(err)
	}
	return fromMessage(msg, latency)
}

func toParams(msgs []llm.Message) ([]sdk.MessageParam, error) {
	out := make([]sdk.MessageParam, 0, len(msgs))
	for i, m := range msgs {
		// Replay assistant turns we produced exactly, including thinking blocks.
		if p, ok := m.ProviderState.(sdk.MessageParam); ok {
			out = append(out, p)
			continue
		}
		blocks := make([]sdk.ContentBlockParamUnion, 0, len(m.Content))
		for j, b := range m.Content {
			switch b.Type {
			case llm.BlockText:
				blocks = append(blocks, sdk.NewTextBlock(b.Text))
			case llm.BlockToolResult:
				blocks = append(blocks, sdk.NewToolResultBlock(b.ToolUseID, b.Text, b.IsError))
			case llm.BlockToolUse:
				blocks = append(blocks, sdk.NewToolUseBlock(b.ToolUseID, b.ToolInput, b.ToolName))
			default:
				return nil, fmt.Errorf("messages[%d].content[%d]: unsupported block type %q", i, j, b.Type)
			}
		}
		switch m.Role {
		case llm.RoleUser:
			out = append(out, sdk.NewUserMessage(blocks...))
		case llm.RoleAssistant:
			out = append(out, sdk.NewAssistantMessage(blocks...))
		default:
			return nil, fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
	}
	return out, nil
}

func toToolParams(tools []llm.Tool) ([]sdk.ToolUnionParam, error) {
	out := make([]sdk.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		var schema map[string]any
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
			return nil, fmt.Errorf("tool %q: invalid input schema: %w", t.Name, err)
		}
		in := sdk.ToolInputSchemaParam{Properties: schema["properties"], ExtraFields: map[string]any{}}
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					in.Required = append(in.Required, s)
				}
			}
		}
		for k, v := range schema {
			if k != "type" && k != "properties" && k != "required" {
				in.ExtraFields[k] = v
			}
		}
		tp := sdk.ToolParam{Name: t.Name, Description: sdk.String(t.Description), InputSchema: in}
		out = append(out, sdk.ToolUnionParam{OfTool: &tp})
	}
	return out, nil
}

func fromMessage(msg *sdk.Message, latency time.Duration) (llm.Response, error) {
	out := llm.Message{Role: llm.RoleAssistant, ProviderState: msg.ToParam()}
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case sdk.TextBlock:
			out.Content = append(out.Content, llm.Block{Type: llm.BlockText, Text: b.Text})
		case sdk.ToolUseBlock:
			out.Content = append(out.Content, llm.Block{
				Type:      llm.BlockToolUse,
				ToolUseID: b.ID,
				ToolName:  b.Name,
				ToolInput: json.RawMessage(b.JSON.Input.Raw()),
			})
		}
		// Thinking blocks are not surfaced; they are preserved in ProviderState.
	}

	return llm.Response{
		Message:    out,
		StopReason: llm.StopReason(msg.StopReason),
		Model:      string(msg.Model),
		Usage:      usage(string(msg.Model), msg.Usage),
		Latency:    latency,
	}, nil
}

func classify(err error) error {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		kind := llm.ErrUnavailable
		switch {
		case apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden:
			kind = llm.ErrAuth
		case apiErr.StatusCode == http.StatusTooManyRequests:
			kind = llm.ErrRateLimited
		case apiErr.StatusCode >= 400 && apiErr.StatusCode < 500:
			kind = llm.ErrInvalidRequest
		}
		msg := string(apiErr.Type())
		if msg == "" {
			msg = http.StatusText(apiErr.StatusCode)
		}
		return &llm.Error{Kind: kind, StatusCode: apiErr.StatusCode, Message: msg, Err: err}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &llm.Error{Kind: llm.ErrTransport, Message: "request cancelled or timed out", Err: err}
	}
	return &llm.Error{Kind: llm.ErrTransport, Message: err.Error(), Err: err}
}
