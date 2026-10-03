package anthropic

import (
	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/ipekutku/ai-sre-agent/internal/llm"
)

// price is USD per million tokens.
type price struct {
	input, output, cacheRead, cacheWrite float64
}

// prices are Anthropic first-party API list prices as of 2026-09-25.
// Cache writes use the 5-minute TTL rate (1.25x input).
// Estimates only; the bill is authoritative.
var prices = map[string]price{
	"claude-sonnet-5-5": {input: 2, output: 10, cacheRead: 0.20, cacheWrite: 2.50},
	"claude-opus-5-5":   {input: 4, output: 20, cacheRead: 0.20, cacheWrite: 5.00},
	"claude-haiku-4-5":  {input: 1, output: 5, cacheRead: 0.10, cacheWrite: 1.25},
}

func usage(model string, u sdk.Usage) llm.Usage {
	out := llm.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
	if p, ok := prices[model]; ok {
		out.CostUSD = (float64(u.InputTokens)*p.input +
			float64(u.OutputTokens)*p.output +
			float64(u.CacheReadInputTokens)*p.cacheRead +
			float64(u.CacheCreationInputTokens)*p.cacheWrite) / 1e6
		out.CostKnown = true
	}
	return out
}
