package helps

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const nonStreamCacheBody = `{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-5",` +
	`"usage":{"input_tokens":2,"cache_creation_input_tokens":35314,"cache_read_input_tokens":161937,` +
	`"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":35314},"output_tokens":7},` +
	`"diagnostics":{"cache_miss_reason":{"type":"messages_changed","cache_missed_input_tokens":25202}}}`

const messageStartLine = `data: {"type":"message_start","message":{"model":"claude-fable-5-1","id":"msg_011",` +
	`"type":"message","role":"assistant","content":[],"usage":{"input_tokens":2,` +
	`"cache_creation_input_tokens":197015,"cache_read_input_tokens":0,` +
	`"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":197015},"output_tokens":4},` +
	`"diagnostics":{"cache_miss_reason":{"type":"tools_changed","cache_missed_input_tokens":90151}}}}`

const messageDeltaLine = `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},` +
	`"usage":{"input_tokens":2,"cache_creation_input_tokens":197015,"cache_read_input_tokens":0,"output_tokens":4}}`

func TestParseClaudeUsageSplitsCacheCreationAndMissReason(t *testing.T) {
	detail := ParseClaudeUsage([]byte(nonStreamCacheBody))

	if detail.CacheReadTokens != 161937 {
		t.Errorf("CacheReadTokens = %d, want 161937", detail.CacheReadTokens)
	}
	if detail.CacheCreationTokens != 35314 {
		t.Errorf("CacheCreationTokens = %d, want 35314", detail.CacheCreationTokens)
	}
	if detail.CacheCreation5mTokens == nil || *detail.CacheCreation5mTokens != 0 {
		t.Errorf("CacheCreation5mTokens = %v, want explicit 0", detail.CacheCreation5mTokens)
	}
	if detail.CacheCreation1hTokens == nil || *detail.CacheCreation1hTokens != 35314 {
		t.Errorf("CacheCreation1hTokens = %v, want 35314", detail.CacheCreation1hTokens)
	}
	if detail.CacheMissReason != "messages_changed" {
		t.Errorf("CacheMissReason = %q, want messages_changed", detail.CacheMissReason)
	}
	if detail.CacheMissedTokens != 25202 {
		t.Errorf("CacheMissedTokens = %d, want 25202", detail.CacheMissedTokens)
	}
	if !detail.TokenBreakdown.Valid() {
		t.Errorf("TokenBreakdown is not valid: %+v", detail.TokenBreakdown)
	}
}

func TestParseClaudeStreamUsageReadsMessageStartDiagnostics(t *testing.T) {
	detail, ok := ParseClaudeStreamUsage([]byte(messageStartLine))
	if !ok {
		t.Fatal("ParseClaudeStreamUsage(message_start) returned false")
	}
	if detail.CacheCreation1hTokens == nil || *detail.CacheCreation1hTokens != 197015 {
		t.Errorf("CacheCreation1hTokens = %v, want 197015", detail.CacheCreation1hTokens)
	}
	if detail.CacheMissReason != "tools_changed" || detail.CacheMissedTokens != 90151 {
		t.Errorf("diagnostics = %q/%d, want tools_changed/90151", detail.CacheMissReason, detail.CacheMissedTokens)
	}

	delta, ok := ParseClaudeStreamUsage([]byte(messageDeltaLine))
	if !ok {
		t.Fatal("ParseClaudeStreamUsage(message_delta) returned false")
	}
	if delta.CacheMissReason != "" {
		t.Errorf("message_delta must not carry diagnostics, got %q", delta.CacheMissReason)
	}
}

func TestClaudeStreamUsageMergesInitialUsageAndCacheDiagnostics(t *testing.T) {
	var buffer StreamUsageBuffer
	for _, line := range []string{
		messageStartLine,
		`data: {"type":"message_delta","usage":{"output_tokens":12}}`,
	} {
		buffer.ObserveClaudeStream([]byte(line))
	}
	detail, ok := buffer.Detail()
	if !ok || detail.InputTokens != 2 || detail.OutputTokens != 12 || detail.TotalTokens != 197029 {
		t.Fatalf("expected initial input and final output usage, got %+v (ok=%v)", detail, ok)
	}
	if detail.CacheCreationTokens != 197015 || detail.CacheCreation1hTokens == nil || *detail.CacheCreation1hTokens != 197015 {
		t.Errorf("cache creation totals and split were not preserved: %+v", detail)
	}
	if detail.CacheMissReason != "tools_changed" || detail.CacheMissedTokens != 90151 {
		t.Errorf("cache miss diagnostics were not preserved: %+v", detail)
	}
	if !detail.TokenBreakdown.Valid() {
		t.Errorf("invalid merged token breakdown: %+v", detail.TokenBreakdown)
	}
}

const openAICachedBody = `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-x",` +
	`"usage":{"prompt_tokens":4096,"completion_tokens":120,"total_tokens":4216,` +
	`"prompt_tokens_details":{"cached_tokens":3584},` +
	`"completion_tokens_details":{"reasoning_tokens":40}}}`

const geminiCachedBody = `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],` +
	`"usageMetadata":{"promptTokenCount":5000,"candidatesTokenCount":80,"thoughtsTokenCount":20,` +
	`"cachedContentTokenCount":4200,"totalTokenCount":5100}}`

const noCacheSignalBody = `{"id":"resp-1","usage":{"prompt_tokens":300,"completion_tokens":15,"total_tokens":315}}`

func TestParseOpenAIUsageReportsCachedTokensAsCacheRead(t *testing.T) {
	detail := ParseOpenAIUsage([]byte(openAICachedBody))

	if detail.CacheReadTokens != 3584 {
		t.Errorf("CacheReadTokens = %d, want 3584", detail.CacheReadTokens)
	}
	if detail.CacheCreationTokens != 0 {
		t.Errorf("CacheCreationTokens = %d, want 0: OpenAI reports no cache creation", detail.CacheCreationTokens)
	}
	if detail.CacheCreation5mTokens != nil || detail.CacheCreation1hTokens != nil {
		t.Errorf("OpenAI must report no pool split, got %v/%v", detail.CacheCreation5mTokens, detail.CacheCreation1hTokens)
	}
	if detail.CacheMissReason != "" {
		t.Errorf("CacheMissReason = %q, want empty", detail.CacheMissReason)
	}
	// prompt_tokens already includes cached_tokens, so the normalized input
	// total must stay 4096 rather than double-counting the cache.
	if got := detail.TokenBreakdown.Input.TotalTokens; got != 4096 {
		t.Errorf("normalized prompt tokens = %d, want 4096", got)
	}
	if got := detail.TokenBreakdown.Input.CacheReadTokens; got != 3584 {
		t.Errorf("normalized cache read = %d, want 3584", got)
	}
	if !detail.TokenBreakdown.Valid() {
		t.Errorf("TokenBreakdown is not valid: %+v", detail.TokenBreakdown)
	}
}

func TestParseGeminiUsageReportsCachedContentAsCacheRead(t *testing.T) {
	detail := ParseGeminiUsage([]byte(geminiCachedBody))

	if detail.CacheReadTokens != 4200 {
		t.Errorf("CacheReadTokens = %d, want 4200", detail.CacheReadTokens)
	}
	if detail.CacheCreationTokens != 0 {
		t.Errorf("CacheCreationTokens = %d, want 0: Gemini reports no cache creation", detail.CacheCreationTokens)
	}
	if detail.CacheCreation5mTokens != nil || detail.CacheCreation1hTokens != nil {
		t.Errorf("Gemini must report no pool split, got %v/%v", detail.CacheCreation5mTokens, detail.CacheCreation1hTokens)
	}
	// cachedContentTokenCount is part of promptTokenCount.
	if got := detail.TokenBreakdown.Input.TotalTokens; got != 5000 {
		t.Errorf("normalized prompt tokens = %d, want 5000", got)
	}
	if got := detail.TokenBreakdown.Input.CacheReadTokens; got != 4200 {
		t.Errorf("normalized cache read = %d, want 4200", got)
	}
}

func TestParseUsageWithoutAnyCacheSignal(t *testing.T) {
	detail := ParseOpenAIUsage([]byte(noCacheSignalBody))

	if detail.CacheReadTokens != 0 || detail.CacheCreationTokens != 0 {
		t.Errorf("cache tokens = %d/%d, want zeros", detail.CacheReadTokens, detail.CacheCreationTokens)
	}
	if got := detail.TokenBreakdown.Input.TotalTokens; got != 300 {
		t.Errorf("normalized prompt tokens = %d, want 300", got)
	}
	if usage.CacheSignalForProvider("xai", "XAIExecutor") != usage.CacheSignalRead {
		t.Error("xAI is OpenAI-compatible and reports cached_tokens")
	}
	if got := usage.CacheSignalForProvider("some-unknown-provider", "MysteryExecutor"); got != usage.CacheSignalNone {
		t.Errorf("unknown provider signal = %q, want none", got)
	}
	if got := usage.CacheSignalForProvider("claude", "ClaudeExecutor"); got != usage.CacheSignalFull {
		t.Errorf("claude signal = %q, want full", got)
	}
	if got := usage.CacheSignalForProvider("gemini", "GeminiExecutor"); got != usage.CacheSignalRead {
		t.Errorf("gemini signal = %q, want read", got)
	}
}
