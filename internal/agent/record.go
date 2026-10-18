package agent

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/ipekutku/ai-sre-agent/internal/tools"
)

// ToolCall describes one registry dispatch. Payloads are owned snapshots of
// the raw input and the registry's size-limited output, not new model context
// or exported telemetry. Output is nil on failure; ErrorCode is empty on success.
type ToolCall struct {
	Seq       int
	Tool      string
	StartedAt time.Time
	Input     json.RawMessage
	Output    json.RawMessage
	ErrorCode tools.ErrorCode
	Duration  time.Duration
}

type toolCallKey struct {
	tool  string
	input string
}

func (r *Result) recordToolCall(name string, input json.RawMessage, started time.Time, result tools.Result, seen map[toolCallKey]struct{}) {
	key := toolCallKey{tool: name, input: canonicalInput(input)}
	if _, exists := seen[key]; exists {
		r.Stats.DuplicateToolCalls++
	}
	seen[key] = struct{}{}
	call := ToolCall{
		Seq: len(r.ToolCalls) + 1, Tool: name, StartedAt: started,
		Input: input, Output: append(json.RawMessage(nil), result.Output...), Duration: result.Duration,
	}
	if result.Err != nil {
		call.ErrorCode = result.Err.Code
	}
	r.ToolCalls = append(r.ToolCalls, call)
}

// canonicalInput ignores object-key order and insignificant whitespace, also
// normalizing JSON string escapes. Arrays retain order and numbers retain their
// exact literals (including precision, exponent and decimal spelling) through
// UseNumber. Malformed JSON uses exact bytes. This is an identity for repetition
// accounting only, not validation or normalization of dispatched arguments.
func canonicalInput(input json.RawMessage) string {
	if !json.Valid(input) {
		return string(input)
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return string(input)
	}
	b, err := json.Marshal(value)
	if err != nil {
		return string(input)
	}
	return string(b)
}
