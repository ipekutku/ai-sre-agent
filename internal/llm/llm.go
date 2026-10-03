// Package llm defines the provider-independent boundary between the
// investigation agent and a large language model.
//
// The agent depends only on Client. Provider implementations (e.g.
// internal/llm/anthropic) translate to and from a specific API.
//
// Conversation rules every caller must follow:
//
//   - Histories are append-only. Append each Response.Message unchanged and
//     never edit, reorder, or remove earlier messages. Providers may attach
//     opaque state (such as reasoning blocks) to a Message that is only valid
//     if the history it was produced in is replayed exactly.
//   - Keep Request.System and Request.Tools identical across all requests of
//     one conversation.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Client generates the next assistant message for a conversation.
type Client interface {
	Generate(ctx context.Context, req Request) (Response, error)
}

// Request is one model call.
type Request struct {
	System   string
	Messages []Message
	Tools    []Tool
}

// Tool describes a tool the model may call.
type Tool struct {
	Name        string
	Description string
	// InputSchema is a JSON Schema object describing the tool's input.
	InputSchema json.RawMessage
}

// Role is the author of a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one conversation turn.
type Message struct {
	Role    Role
	Content []Block
	// ProviderState is set by a provider on messages it returns. It must be
	// passed back unchanged; providers use it to replay the turn exactly.
	ProviderState any
}

// BlockType identifies the kind of content block.
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
)

// Block is a piece of message content.
type Block struct {
	Type BlockType

	// Text is set for BlockText, and holds the result content for BlockToolResult.
	Text string

	// ToolUseID links a tool_use block to its tool_result.
	ToolUseID string
	// ToolName and ToolInput are set for BlockToolUse.
	ToolName  string
	ToolInput json.RawMessage
	// IsError marks a BlockToolResult as a failed tool call.
	IsError bool
}

// UserText returns a user message containing text.
func UserText(text string) Message {
	return Message{Role: RoleUser, Content: []Block{{Type: BlockText, Text: text}}}
}

// ToolResult returns a tool_result block for the tool_use with the given ID.
func ToolResult(toolUseID, content string, isError bool) Block {
	return Block{Type: BlockToolResult, ToolUseID: toolUseID, Text: content, IsError: isError}
}

// StopReason is why the model stopped generating.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	// StopRefusal means the provider declined to answer.
	StopRefusal StopReason = "refusal"
)

// Response is the model's reply.
type Response struct {
	// Message is the assistant turn. Append it to the history unchanged.
	Message    Message
	StopReason StopReason
	// Model is the model that actually served the request.
	Model   string
	Usage   Usage
	Latency time.Duration
}

// ToolUses returns the tool_use blocks in the response.
func (r Response) ToolUses() []Block {
	var out []Block
	for _, b := range r.Message.Content {
		if b.Type == BlockToolUse {
			out = append(out, b)
		}
	}
	return out
}

// Text returns the concatenated text blocks in the response.
func (r Response) Text() string {
	var s string
	for _, b := range r.Message.Content {
		if b.Type == BlockText {
			s += b.Text
		}
	}
	return s
}

// Usage is token accounting for one request.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	// CostUSD is the estimated cost. CostKnown is false if the provider has
	// no price for the model.
	CostUSD   float64
	CostKnown bool
}

// Add returns the sum of u and v. The cost is known only if both are.
func (u Usage) Add(v Usage) Usage {
	return Usage{
		InputTokens:      u.InputTokens + v.InputTokens,
		OutputTokens:     u.OutputTokens + v.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens + v.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + v.CacheWriteTokens,
		CostUSD:          u.CostUSD + v.CostUSD,
		CostKnown:        u.CostKnown && v.CostKnown,
	}
}

// ErrorKind classifies a failed model call.
type ErrorKind string

const (
	// ErrInvalidRequest: the request was malformed (not retryable).
	ErrInvalidRequest ErrorKind = "invalid_request"
	// ErrAuth: missing or rejected credentials (not retryable).
	ErrAuth ErrorKind = "auth"
	// ErrRateLimited: rate limit hit after the provider's own retries.
	ErrRateLimited ErrorKind = "rate_limited"
	// ErrUnavailable: provider overloaded or server error after retries.
	ErrUnavailable ErrorKind = "unavailable"
	// ErrTransport: network failure or timeout.
	ErrTransport ErrorKind = "transport"
)

// Error is a classified model-call failure. Its message must not contain
// credentials.
type Error struct {
	Kind       ErrorKind
	StatusCode int // 0 if no HTTP response
	Message    string
	Err        error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.StatusCode != 0 {
		return fmt.Sprintf("llm %s (HTTP %d): %s", e.Kind, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("llm %s: %s", e.Kind, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf returns the ErrorKind of err, or "" if err is not an *Error.
func KindOf(err error) ErrorKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}
