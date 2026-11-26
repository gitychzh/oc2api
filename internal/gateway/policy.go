package gateway

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"opencode2api/internal/jsonutil"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
)

// The free tier's prompt gate, enforced locally.
//
// Four free ids answer 400 above round(chars * 0.33966) > 32000 — a character
// rule that no token window can express (the advertised 200k window admits
// 8.5x more than the gate). Forwarding an oversized prompt therefore costs a
// round trip plus an egress lease on an exit that then has to be cooled, and
// the client learns nothing the gateway could not have said instantly.
//
// The refusal is deliberately shaped like the vendor's own: opencode starts a
// compaction when it sees code=context_length_exceeded, so a gateway-only
// shape would make the CLI retry the identical oversized prompt forever.

// charCount returns the number of characters in s, counted as Unicode code
// points rather than bytes.
//
// This matters: the free tier's gate counts characters, and it was bisected
// with natural Chinese text. Counting bytes would bill a CJK character at 3 and
// a 4-byte symbol at 4, so the same conversation would hit the gate at a third
// of its real length — refusing prompts the upstream accepts, which is exactly
// the failure mode the gate is supposed to prevent.
func charCount(s string) int {
	return utf8.RuneCountInString(s)
}

// promptCharsOf counts the characters the free tier's gate bills for one
// request body. The gate is content-agnostic, so this total — not a token
// estimate — is what decides whether the upstream accepts the prompt.
//
// Messages and tools are counted the way the vendor counts them: message text
// and tool-call arguments, plus each tool's name, description and compact JSON
// schema. Injected tool definitions count too, which is why a client with many
// tools has less conversation budget than one with few.
func promptCharsOf(payload map[string]any, protocol wire.Protocol) (total, toolChars int) {
	messages := requestMessages(payload, protocol)
	for _, message := range messages {
		object, ok := message.(map[string]any)
		if !ok {
			continue
		}
		total += charCount(contentText(object["content"]))
		total += charCount(jsonutil.StringAt(object, "reasoning_content"))
		if calls, ok := object["tool_calls"].([]any); ok {
			for _, call := range calls {
				callObject, ok := call.(map[string]any)
				if !ok {
					continue
				}
				total += charCount(jsonutil.StringAt(callObject, "function", "name"))
				total += charCount(jsonutil.StringAt(callObject, "function", "arguments"))
			}
		}
		// Anthropic carries tool calls and results as content blocks; the
		// text extractor above already covers their text, and the input JSON
		// is counted through the tool_use block's input object.
		if blocks, ok := object["content"].([]any); ok {
			for _, block := range blocks {
				blockObject, ok := block.(map[string]any)
				if !ok {
					continue
				}
				if jsonutil.StringAt(blockObject, "type") == "tool_use" {
					if input := blockObject["input"]; input != nil {
						if encoded, err := jsonutil.Marshal(input); err == nil {
							total += charCount(string(encoded))
						}
					}
				}
			}
		}
	}
	tools, _ := payload["tools"].([]any)
	for _, tool := range tools {
		toolObject, ok := tool.(map[string]any)
		if !ok {
			continue
		}
		payloadObject := toolObject
		if fn, ok := toolObject["function"].(map[string]any); ok {
			payloadObject = fn
		}
		toolChars += charCount(jsonutil.StringAt(payloadObject, "name"))
		toolChars += charCount(jsonutil.StringAt(payloadObject, "description"))
		parameters := payloadObject["parameters"]
		if parameters == nil {
			parameters = payloadObject["input_schema"]
		}
		if parameters != nil {
			if encoded, err := jsonutil.Marshal(parameters); err == nil {
				toolChars += charCount(string(encoded))
			}
		}
	}
	return total + toolChars, toolChars
}

// requestMessages returns the message list of payload in whichever key the
// protocol uses. Responses carries a bare "input" string for the simple form,
// which is counted as a single user message.
func requestMessages(payload map[string]any, protocol wire.Protocol) []any {
	if messages, ok := payload["messages"].([]any); ok {
		return messages
	}
	if protocol == wire.Responses {
		switch input := payload["input"].(type) {
		case []any:
			return input
		case string:
			return []any{map[string]any{"role": "user", "content": input}}
		}
	}
	return nil
}

// contentText flattens an OpenAI or Anthropic message content value to its
// text. Part lists keep every string field that carries text; images and
// files contribute nothing, since the gate bills characters.
func contentText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var builder strings.Builder
		for _, part := range value {
			object, ok := part.(map[string]any)
			if !ok {
				if text, ok := part.(string); ok {
					builder.WriteString(text)
				}
				continue
			}
			for _, key := range []string{"text", "content"} {
				if text, ok := object[key].(string); ok {
					builder.WriteString(text)
				}
			}
		}
		return builder.String()
	}
	return ""
}

// contextOverflow is the local refusal for a prompt past the free tier's
// measured character gate. It carries the numbers so the reply can be acted
// on instead of guessed at.
type contextOverflow struct {
	model        string
	estimated    int
	contextLimit int
	inputLimit   int
	ceiling      int
	promptChars  int
	gateChars    int
	toolChars    int
}

// Error renders the refusal. The leading sentence is byte-identical to the
// vendor's own 400 so a client that already parses it needs no new branch, and
// the trailing clause names the real constraint, because the vendor's sentence
// advertises the model window while the free tier can never send that much. A
// client that budgets against the advertised ceiling compacts forever without
// ever getting under the gate.
func (e contextOverflow) Error() string {
	ceiling := e.ceiling
	if ceiling <= 0 {
		ceiling = e.inputLimit
	}
	if e.gateChars > 0 && e.inputLimit > 0 && e.inputLimit < ceiling {
		// The reachable compaction point is the gate, not the window.
		ceiling = e.inputLimit
	}
	base := fmt.Sprintf(
		"This model's maximum context length is %d tokens and at most %d prompt tokens are accepted. "+
			"This request is about %d prompt tokens. Reduce the prompt or compact the conversation "+
			"(the client compacts below %d tokens) and retry.",
		e.contextLimit, e.inputLimit, e.estimated, ceiling,
	)
	if e.gateChars <= 0 {
		return base
	}
	messageChars := e.promptChars - e.toolChars
	if messageChars < 0 {
		messageChars = 0
	}
	excess := e.promptChars - e.gateChars
	if excess < 0 {
		excess = 0
	}
	return base + " [opencode2api] The free tier enforces a " + strconv.Itoa(e.gateChars) +
		"-character prompt cap, not the advertised token window: this request carried " +
		strconv.Itoa(e.promptChars) + " characters (" + strconv.Itoa(e.toolChars) +
		" in tool definitions, " + strconv.Itoa(messageChars) + " in messages). Trim at least " +
		strconv.Itoa(excess) + " characters — tool definitions count against the same cap, so " +
		"disabling unused tools is often the cheapest way to get under it."
}

// promptTextOf flattens every model-visible string of a request into one
// buffer, for density-weighted token estimation.
func promptTextOf(payload map[string]any, protocol wire.Protocol) string {
	var builder strings.Builder
	for _, message := range requestMessages(payload, protocol) {
		object, ok := message.(map[string]any)
		if !ok {
			continue
		}
		builder.WriteString(contentText(object["content"]))
		builder.WriteString(jsonutil.StringAt(object, "reasoning_content"))
		if calls, ok := object["tool_calls"].([]any); ok {
			for _, call := range calls {
				callObject, ok := call.(map[string]any)
				if !ok {
					continue
				}
				builder.WriteString(jsonutil.StringAt(callObject, "function", "name"))
				builder.WriteString(jsonutil.StringAt(callObject, "function", "arguments"))
			}
		}
	}
	if tools, ok := payload["tools"].([]any); ok {
		if encoded, err := jsonutil.Marshal(tools); err == nil {
			builder.Write(encoded)
		}
	}
	return builder.String()
}

// estimatePromptTokens estimates the prompt tokens the vendor's own tokenizer
// would bill for payload. The free tier's streaming replies carry no usage
// frame at all, so without this a streaming client reads zero tokens for the
// whole session and cannot see how close it is to the prompt gate.
func estimatePromptTokens(payload map[string]any, protocol wire.Protocol) int {
	return models.EstimateBilledPromptTokens(promptTextOf(payload, protocol))
}

// enforcePromptGate refuses payload locally when it is past the measured
// character gate for model. It returns nil when the request may be sent.
//
// Only models with a bisected gate are enforced; an unknown model is served
// with no character limit at all, because inventing one refuses prompts the
// upstream would have accepted.
func enforcePromptGate(payload map[string]any, model string, protocol wire.Protocol) error {
	gate := models.PromptGateFor(model)
	if gate.Chars <= 0 {
		return nil
	}
	chars, toolChars := promptCharsOf(payload, protocol)
	if chars <= gate.Chars {
		return nil
	}
	contextLimit := gate.Context
	if contextLimit <= 0 {
		contextLimit = gate.Chars
	}
	inputLimit := gate.Input
	if inputLimit <= 0 {
		// The vendor's message names the prompt cap; without a declared input
		// limit, report the gate's own token equivalent.
		inputLimit = models.GatePromptTokens
	}
	ceiling := models.CompactionCeiling(gate.Input)
	if ceiling <= 0 {
		ceiling = models.CompactionCeiling(gate.Context)
	}
	return contextOverflow{
		model:        model,
		estimated:    int(float64(chars) * models.GateCharsRatio),
		contextLimit: contextLimit,
		inputLimit:   inputLimit,
		ceiling:      ceiling,
		promptChars:  chars,
		gateChars:    gate.Chars,
		toolChars:    toolChars,
	}
}
