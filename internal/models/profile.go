package models

import wire "opencode2api/internal/protocol"

// Measured model parameters and the free tier's prompt gate.
//
// The gateway stands in for the provider, so the numbers it enforces must be
// the provider's numbers. Everything in this file is transcribed from a live
// measurement or from the upstream catalogue; nothing is invented. A model
// with no profile entry is served with no enforced limit at all, because
// inventing a ceiling for an unknown model is how a gateway starts refusing
// traffic that used to work.
//
// The table itself lives in the documentation (docs/MODEL_PROFILE.md), which
// records the measurement behind every row. This file holds only what the
// runtime needs: the free tier's character gate, and the token window each
// known free model advertises.

// The free tier's prompt gate.
//
// Four ids were bisected twice (2026-11-17 and 2026-11-18) with identical
// results: 92,675 characters are accepted and 94,442 are rejected, for every
// one of them. The upstream's own 400 says
//
//	This model's maximum context length is 200000 tokens and at most 32000
//	prompt tokens are accepted. This request is about 32054 prompt tokens.
//
// where "about" is round(chars * 0.33966) regardless of language or content
// type — CJK, Latin and code all bill the same per character. The binding
// limit is therefore a character count, not a token window: the advertised
// 200k window admits 8.5x more than the gate.
//
// Enforcing it locally costs one instant 400 instead of a round trip plus an
// egress lease on an exit that then has to be cooled.
const (
	// GatePromptTokens is the "at most N prompt tokens" the upstream reports.
	GatePromptTokens = 32_000
	// GateCharsRatio converts a character count to the upstream's "about N
	// prompt tokens" estimate.
	GateCharsRatio = 0.33966
	// GatePromptChars is the measured character ceiling: the largest prompt
	// the gate admits. 92,675 was accepted and 94,442 rejected, so this sits
	// just under the edge.
	GatePromptChars = 94_200
)

// Client-estimator constants, mirroring the OpenCode CLI.
const (
	// CharsPerToken is the estimator both the CLI and this gateway use for
	// the token window: round(len/4).
	CharsPerToken = 4
	// ReserveShare and ReserveMin reproduce the CLI's auto-compaction
	// ceiling: the client compacts below input - max(15% of the window, 16k).
	ReserveShare = 0.15
	ReserveMin   = 16_000
)

// PromptGate is the measured free-tier character gate for one model.
type PromptGate struct {
	// Chars is the largest prompt the gate admits, in characters counted over
	// the whole request including injected tool definitions. Zero means this
	// model's gate has never been bisected, so nothing is enforced beyond the
	// token window.
	Chars int
	// Context is the model's advertised total window, echoed in the refusal so
	// it reads like the vendor's own message.
	Context int
	// Input is the advertised prompt limit, used as the compaction point when
	// the model declares one.
	Input int
}

// promptGates holds the measured character gate per upstream model ID.
//
// Only the four ids that were actually bisected appear here. The newer free
// ids are deliberately absent: hy4-flash-free is not subject to this gate at
// all (it answers to its real 1,048,576-token window, measured at 1,281,215
// natural CJK characters), and the remaining ids have never been bisected.
// Giving them the 94,200-character ceiling would refuse prompts the upstream
// would have accepted.
var promptGates = map[string]PromptGate{
	"big-pickle":               {Chars: GatePromptChars, Context: 262_139, Input: 261_000},
	"deepseek-v4.1-flash-free": {Chars: GatePromptChars, Context: 200_000},
	"glm-5.3-flash-free":       {Chars: GatePromptChars, Context: 200_000},
	"minimax-m3.1-flash-free":  {Chars: GatePromptChars, Context: 1_000_000},
}

// PromptGateFor returns the measured gate for model, tolerating an
// "opencode/" provider prefix the way the upstream catalogue does. The zero
// value means no gate is known for this model.
func PromptGateFor(model string) PromptGate {
	if gate, ok := promptGates[model]; ok {
		return gate
	}
	if _, tail, found := cutPrefix(model); found {
		if gate, ok := promptGates[tail]; ok {
			return gate
		}
	}
	return PromptGate{}
}

// cutPrefix splits "provider/id" into its parts, reporting whether the string
// contained a separator at all.
func cutPrefix(model string) (prefix, tail string, found bool) {
	for index := range len(model) {
		if model[index] == '/' {
			return model[:index], model[index+1:], true
		}
	}
	return "", model, false
}

// CompactionCeiling is the first prompt size at which the CLI compacts:
// window - max(15% of the window, 16k), where the window is the input limit
// when the catalogue states one.
func CompactionCeiling(window int) int {
	if window <= 0 {
		return 0
	}
	reserve := int(float64(window) * ReserveShare)
	if reserve < ReserveMin {
		reserve = ReserveMin
	}
	if ceiling := window - reserve; ceiling > 0 {
		return ceiling
	}
	return 0
}

// EstimateTokens is the CLI's prompt estimator: round(chars/4).
func EstimateTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return (chars + CharsPerToken/2) / CharsPerToken
}

// EstimateBilledPromptTokens estimates the prompt tokens the vendor's own
// tokenizer would bill for text, for the usage frame a streaming reply is
// missing. The density constants live in internal/protocol, which owns the
// stream shape this estimate feeds.
func EstimateBilledPromptTokens(text string) int {
	return wire.EstimatePromptTokens(text)
}
