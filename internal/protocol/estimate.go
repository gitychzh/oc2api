package protocol

// Token density for estimating a usage frame the upstream did not send.
//
// The free tier's streaming replies carry no usage at all: eight content
// chunks, a terminal finish_reason, and no choices:[] usage frame (measured
// 2026-11-18). A streaming client therefore reads zero tokens for the whole
// session and cannot see how close it is to the prompt gate. The gateway
// synthesises one frame from these ratios.
//
// They are billed-token ratios, not the free tier's gate ratio: the vendor's
// own tokenizer bills 0.67-0.89 tokens per natural CJK character, so an
// estimate built on these lands within a few percent of the real bill instead
// of the 2.7x error a chars/4 estimator would produce.
const (
	// CJKTokenRatio is billed prompt tokens per CJK character. Two bisections
	// of the same 92,675 characters of natural Chinese billed 62,807 and
	// 71,238, so 0.70 is the midpoint of a ~13% spread.
	CJKTokenRatio = 0.70
	// ASCIITokenRatio is billed prompt tokens per ASCII/code character.
	ASCIITokenRatio = 0.28
	// PromptOverheadTokens covers role markers, the tool JSON wrapper and the
	// chat template.
	PromptOverheadTokens = 300
	// CompletionTokenRatio is billed completion tokens per generated
	// character for CJK-heavy answers.
	CompletionTokenRatio = 0.6
)

// EstimatePromptTokens estimates the prompt tokens the vendor's tokenizer
// would bill for text.
func EstimatePromptTokens(text string) int {
	runes := []rune(text)
	cjk := 0
	for _, r := range runes {
		if isCJK(r) {
			cjk++
		}
	}
	other := len(runes) - cjk
	return int(float64(cjk)*CJKTokenRatio + float64(other)*ASCIITokenRatio + PromptOverheadTokens)
}

// EstimateCompletionTokens estimates billed completion tokens for the given
// number of generated characters.
func EstimateCompletionTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return int(float64(chars) * CompletionTokenRatio)
}

// isCJK reports whether r is billed near one token per character by the
// vendor's tokenizer.
func isCJK(r rune) bool {
	switch {
	case r >= 0x3000 && r <= 0x303F, // CJK punctuation
		r >= 0x3400 && r <= 0x4DBF, // extension A
		r >= 0x4E00 && r <= 0x9FFF, // unified ideographs
		r >= 0xF900 && r <= 0xFAFF, // compatibility ideographs
		r >= 0xFF00 && r <= 0xFF60, // fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6:
		return true
	}
	return false
}
