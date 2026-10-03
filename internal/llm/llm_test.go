package llm

import (
	"errors"
	"fmt"
	"testing"
)

func TestUsageAdd(t *testing.T) {
	a := Usage{InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4, CostUSD: 0.5, CostKnown: true}
	b := Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 40, CostUSD: 0.25, CostKnown: true}
	got := a.Add(b)
	want := Usage{InputTokens: 11, OutputTokens: 22, CacheReadTokens: 33, CacheWriteTokens: 44, CostUSD: 0.75, CostKnown: true}
	if got != want {
		t.Errorf("Add = %+v, want %+v", got, want)
	}
	if a.Add(Usage{}).CostKnown {
		t.Error("cost must be unknown if any part is unknown")
	}
}

func TestErrorKindAndFormatting(t *testing.T) {
	err := fmt.Errorf("investigate: %w", &Error{Kind: ErrRateLimited, StatusCode: 429, Message: "rate_limit_error"})
	if KindOf(err) != ErrRateLimited {
		t.Errorf("KindOf = %q", KindOf(err))
	}
	if KindOf(errors.New("other")) != "" {
		t.Error("KindOf non-llm error should be empty")
	}
	var nilErr *Error
	if fmt.Sprintf("%v", nilErr) != "<nil>" {
		t.Error("nil *Error must format safely")
	}
}

func TestResponseHelpers(t *testing.T) {
	r := Response{Message: Message{Content: []Block{
		{Type: BlockText, Text: "a"},
		{Type: BlockToolUse, ToolUseID: "1"},
		{Type: BlockText, Text: "b"},
	}}}
	if r.Text() != "ab" || len(r.ToolUses()) != 1 {
		t.Errorf("Text = %q, ToolUses = %v", r.Text(), r.ToolUses())
	}
}
