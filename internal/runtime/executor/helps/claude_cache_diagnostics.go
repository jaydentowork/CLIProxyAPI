package helps

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
)

// claudeCacheDiagnosticsFromRoot reads the diagnostics.cache_miss_reason object
// a Claude body (or message_start message) carries beside its usage node.
func claudeCacheDiagnosticsFromRoot(root gjson.Result) (string, int64) {
	reason := root.Get("diagnostics.cache_miss_reason")
	if !reason.Exists() {
		return "", 0
	}
	return strings.TrimSpace(reason.Get("type").String()), reason.Get("cache_missed_input_tokens").Int()
}

// ExtractClaudeCacheMissReason reads the cache-miss diagnostics a Claude response
// carries when the account has the cache-diagnosis beta.
func ExtractClaudeCacheMissReason(payload []byte) (string, int64) {
	if len(payload) == 0 {
		return "", 0
	}
	if gjson.ValidBytes(payload) {
		root := gjson.ParseBytes(payload)
		// Non-streaming: diagnostics sits beside usage at the top level.
		if reason, tokens := claudeCacheDiagnosticsFromRoot(root); reason != "" || tokens != 0 {
			return reason, tokens
		}
		// A whole message_start object handed over directly.
		if reason, tokens := claudeCacheDiagnosticsFromRoot(root.Get("message")); reason != "" || tokens != 0 {
			return reason, tokens
		}
		return "", 0
	}
	// Streaming: scan the SSE for the message_start event that carries it.
	for _, line := range bytes.Split(payload, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		event := bytes.TrimSpace(line[len("data:"):])
		if len(event) == 0 || !gjson.ValidBytes(event) {
			continue
		}
		root := gjson.ParseBytes(event)
		if root.Get("type").String() != "message_start" {
			continue
		}
		if reason, tokens := claudeCacheDiagnosticsFromRoot(root.Get("message")); reason != "" || tokens != 0 {
			return reason, tokens
		}
	}
	return "", 0
}

// RequestMaxTokens reads the generation cap from the first payload that carries
// one, trying the Anthropic name and the two OpenAI-family spellings so a
// translated request still reports the cap the client asked for.
func RequestMaxTokens(payloads ...[]byte) int64 {
	for _, payload := range payloads {
		if len(payload) == 0 {
			continue
		}
		for _, field := range []string{"max_tokens", "max_output_tokens", "max_completion_tokens"} {
			if node := gjson.GetBytes(payload, field); node.Exists() {
				if value := node.Int(); value > 0 {
					return value
				}
			}
		}
	}
	return 0
}
