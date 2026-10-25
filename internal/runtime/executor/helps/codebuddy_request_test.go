package helps

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
)

func TestPrepareCodeBuddyRequestBodyForcesStreamAndNormalizesToolChoice(t *testing.T) {
	cases := []struct {
		name           string
		body           string
		wantStream     bool
		wantInclude    bool
		wantToolChoice string
	}{
		{
			name:           "object tool choice with name becomes the function name",
			body:           `{"model":"gpt-5","stream":false,"tool_choice":{"type":"function","function":{"name":"lookup"}}}`,
			wantStream:     true,
			wantInclude:    true,
			wantToolChoice: "lookup",
		},
		{
			name:           "object tool choice without name becomes required",
			body:           `{"model":"gpt-5","tool_choice":{"type":"function"}}`,
			wantStream:     true,
			wantInclude:    true,
			wantToolChoice: "required",
		},
		{
			name:           "string tool choice is preserved",
			body:           `{"model":"gpt-5","tool_choice":"none"}`,
			wantStream:     true,
			wantInclude:    true,
			wantToolChoice: "none",
		},
		{
			name:           "existing include_usage is preserved",
			body:           `{"model":"gpt-5","stream_options":{"include_usage":false}}`,
			wantStream:     true,
			wantInclude:    false,
			wantToolChoice: "",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			out, err := PrepareCodeBuddyRequestBody([]byte(tt.body))
			if err != nil {
				t.Fatalf("PrepareCodeBuddyRequestBody() error = %v", err)
			}
			if got := gjson.GetBytes(out, "stream").Bool(); got != tt.wantStream {
				t.Errorf("stream = %v, want %v", got, tt.wantStream)
			}
			includeUsage := gjson.GetBytes(out, "stream_options.include_usage")
			if !includeUsage.Exists() {
				t.Fatal("stream_options.include_usage is missing")
			}
			if includeUsage.Bool() != tt.wantInclude {
				t.Errorf("stream_options.include_usage = %v, want %v", includeUsage.Bool(), tt.wantInclude)
			}
			if choice := gjson.GetBytes(out, "tool_choice"); choice.Exists() && choice.Type == gjson.String {
				if tt.wantToolChoice == "" {
					t.Fatalf("tool_choice unexpectedly rewritten to %q", choice.String())
				}
				if choice.String() != tt.wantToolChoice {
					t.Errorf("tool_choice = %q, want %q", choice.String(), tt.wantToolChoice)
				}
			} else if tt.wantToolChoice != "" {
				t.Errorf("tool_choice is not a string: %s", gjson.GetBytes(out, "tool_choice").Raw)
			}
			if got := gjson.GetBytes(out, "model").String(); got != "gpt-5" {
				t.Errorf("model = %q, want gpt-5", got)
			}
		})
	}
}

func TestPrepareCodeBuddyRequestBodyLeavesInvalidPayloadAlone(t *testing.T) {
	body := []byte("not json")
	out, err := PrepareCodeBuddyRequestBody(body)
	if err != nil {
		t.Fatalf("PrepareCodeBuddyRequestBody() error = %v", err)
	}
	if string(out) != string(body) {
		t.Fatalf("out = %q, want %q", out, body)
	}
}

func TestCodeBuddyConversationSeed(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "string content",
			body: `{"messages":[{"role":"user","content":"hello world"}]}`,
			want: "hello world",
		},
		{
			name: "content parts are compacted with sorted keys",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"text":"there","type":"text"}]}]}`,
			want: `[{"text":"hi","type":"text"},{"text":"there","type":"text"}]`,
		},
		{
			name: "empty messages fall back to the whole body",
			body: `{"messages":[]}`,
			want: `{"messages":[]}`,
		},
		{
			name: "no messages falls back to the whole body",
			body: `{"model":"gpt-5"}`,
			want: `{"model":"gpt-5"}`,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := CodeBuddyConversationSeed([]byte(tt.body)); got != tt.want {
				t.Fatalf("CodeBuddyConversationSeed() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSanitizeCodeBuddyChunkRemovesEmptyFinishReasonAndBackfillsToolName(t *testing.T) {
	state := NewCodeBuddyToolCallNameState()

	first := []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]},"finish_reason":null}]}`)
	out, err := SanitizeCodeBuddyChunk(first, state)
	if err != nil {
		t.Fatalf("SanitizeCodeBuddyChunk() error = %v", err)
	}
	if gjson.GetBytes(out, "choices.0.finish_reason").Exists() {
		t.Error("null finish_reason was not removed")
	}
	if got := gjson.GetBytes(out, "choices.0.delta.tool_calls.0.function.name").String(); got != "lookup" {
		t.Errorf("tool name = %q, want lookup", got)
	}

	second := []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":"1}"}}]},"finish_reason":""}]}`)
	out, err = SanitizeCodeBuddyChunk(second, state)
	if err != nil {
		t.Fatalf("SanitizeCodeBuddyChunk() error = %v", err)
	}
	if gjson.GetBytes(out, "choices.0.finish_reason").Exists() {
		t.Error("empty finish_reason was not removed")
	}
	if got := gjson.GetBytes(out, "choices.0.delta.tool_calls.0.function.name").String(); got != "lookup" {
		t.Errorf("backfilled tool name = %q, want lookup", got)
	}
	if got := gjson.GetBytes(out, "choices.0.delta.tool_calls.0.function.arguments").String(); got != "1}" {
		t.Errorf("arguments = %q, want 1}", got)
	}
}

func TestSanitizeCodeBuddyChunkKeepsRealFinishReason(t *testing.T) {
	body := []byte(`{"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}`)
	out, err := SanitizeCodeBuddyChunk(body, NewCodeBuddyToolCallNameState())
	if err != nil {
		t.Fatalf("SanitizeCodeBuddyChunk() error = %v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "stop" {
		t.Fatalf("finish_reason = %q, want stop", got)
	}
}

func TestCodeBuddyChunkDataLine(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{line: "data: {\"a\":1}", want: true},
		{line: "data:{\"a\":1}", want: true},
		{line: "data: [DONE]", want: false},
		{line: ": keep-alive", want: false},
		{line: "event: message", want: false},
		{line: "", want: false},
	}
	for _, tt := range cases {
		payload, ok := CodeBuddyChunkDataLine([]byte(tt.line))
		if ok != tt.want {
			t.Errorf("CodeBuddyChunkDataLine(%q) ok = %v, want %v", tt.line, ok, tt.want)
			continue
		}
		if ok && !json.Valid(payload) {
			t.Errorf("CodeBuddyChunkDataLine(%q) returned invalid JSON %q", tt.line, payload)
		}
	}
}

func TestCodeBuddyStreamAggregatorFoldsChunks(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"content":"lo","reasoning_content":"think"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":"\"x\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-5","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
		`data: [DONE]`,
	}, "\n")

	aggregated := CodeBuddyStreamAggregatorForSSE([]byte(stream), NewCodeBuddyStreamProcessor())
	if len(aggregated) == 0 {
		t.Fatal("CodeBuddyStreamAggregatorForSSE() returned no payload")
	}

	if got := gjson.GetBytes(aggregated, "object").String(); got != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", got)
	}
	if got := gjson.GetBytes(aggregated, "choices.0.message.content").String(); got != "Hello" {
		t.Errorf("content = %q, want Hello", got)
	}
	if got := gjson.GetBytes(aggregated, "choices.0.message.reasoning_content").String(); got != "think" {
		t.Errorf("reasoning_content = %q, want think", got)
	}
	if got := gjson.GetBytes(aggregated, "choices.0.message.tool_calls.0.function.name").String(); got != "lookup" {
		t.Errorf("tool name = %q, want lookup", got)
	}
	if got := gjson.GetBytes(aggregated, "choices.0.message.tool_calls.0.function.arguments").String(); got != `{"q":"x"}` {
		t.Errorf("tool arguments = %q, want {\"q\":\"x\"}", got)
	}
	if got := gjson.GetBytes(aggregated, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", got)
	}
	if got := gjson.GetBytes(aggregated, "usage.total_tokens").Int(); got != 18 {
		t.Errorf("usage.total_tokens = %d, want 18", got)
	}
	if got := gjson.GetBytes(aggregated, "choices.0.message.role").String(); got != "assistant" {
		t.Errorf("role = %q, want assistant", got)
	}
}

func TestCodeBuddyStreamAggregatorDropsUnnamedToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		`data: [DONE]`,
	}, "\n")

	aggregated := CodeBuddyStreamAggregatorForSSE([]byte(stream), NewCodeBuddyStreamProcessor())
	if len(aggregated) == 0 {
		t.Fatal("CodeBuddyStreamAggregatorForSSE() returned no payload")
	}
	if gjson.GetBytes(aggregated, "choices.0.message.tool_calls").Exists() {
		t.Fatalf("unnamed tool call was kept: %s", aggregated)
	}
	if got := gjson.GetBytes(aggregated, "choices.0.finish_reason").String(); got != "stop" {
		t.Errorf("finish_reason = %q, want stop", got)
	}
}

func TestCodeBuddyStreamProcessorRecoversSplitMarkupAsToolCalls(t *testing.T) {
	processor := NewCodeBuddyStreamProcessor()

	first := []byte(`{"choices":[{"index":0,"delta":{"content":"先看目录：<tool_ca"},"finish_reason":null}]}`)
	out, keep, err := processor.SanitizeChunk(first, false)
	if err != nil {
		t.Fatalf("SanitizeChunk() error = %v", err)
	}
	if !keep {
		t.Fatal("the chunk carrying visible text was dropped")
	}
	if got := gjson.GetBytes(out, "choices.0.delta.content").String(); got != "先看目录：" {
		t.Fatalf("content = %q, want the text before the partial markup", got)
	}
	if gjson.GetBytes(out, "choices.0.delta.tool_calls").Exists() {
		t.Fatal("a partial markup chunk must not produce a tool call")
	}

	second := []byte(`{"choices":[{"index":0,"delta":{"content":"lls><invoke name=\"bash\"><parameter name=\"cmd\">pwd</parameter></invoke></tool_calls>"},"finish_reason":null}]}`)
	out, keep, err = processor.SanitizeChunk(second, false)
	if err != nil {
		t.Fatalf("SanitizeChunk() error = %v", err)
	}
	if !keep {
		t.Fatal("the chunk completing a tool call was dropped")
	}
	if got := gjson.GetBytes(out, "choices.0.delta.content").String(); got != "" {
		t.Errorf("content = %q, want the markup removed", got)
	}
	calls := gjson.GetBytes(out, "choices.0.delta.tool_calls")
	if !calls.IsArray() || len(calls.Array()) != 1 {
		t.Fatalf("delta.tool_calls = %s, want one recovered tool call", calls.Raw)
	}
	if got := calls.Array()[0].Get("function.name").String(); got != "bash" {
		t.Errorf("tool name = %q, want bash", got)
	}
	if got := calls.Array()[0].Get("function.arguments").String(); got != `{"cmd":"pwd"}` {
		t.Errorf("tool arguments = %q, want {\"cmd\":\"pwd\"}", got)
	}
	if got := calls.Array()[0].Get("type").String(); got != "function" {
		t.Errorf("tool type = %q, want function", got)
	}
	if got := calls.Array()[0].Get("id").String(); got == "" {
		t.Error("recovered tool call has no id")
	}
	if gjson.GetBytes(out, "choices.0.finish_reason").Exists() {
		t.Errorf("a mid-stream chunk must not carry a finish reason: %s", out)
	}

	stop := []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	stopOut, keepStop, errStop := processor.SanitizeChunk(stop, false)
	if errStop != nil {
		t.Fatalf("SanitizeChunk(stop) error = %v", errStop)
	}
	if !keepStop {
		t.Fatal("the terminal chunk was dropped")
	}
	if got := gjson.GetBytes(stopOut, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Errorf("terminal finish_reason = %q, want tool_calls", got)
	}
}

func TestCodeBuddyStreamProcessorRewritesTerminalStopAfterRecoveredCalls(t *testing.T) {
	processor := NewCodeBuddyStreamProcessor()

	markup := []byte(`{"choices":[{"index":0,"delta":{"content":"<invoke name=\"bash\"><parameter name=\"cmd\">pwd</parameter></invoke>"}}]}`)
	if _, _, err := processor.SanitizeChunk(markup, false); err != nil {
		t.Fatalf("SanitizeChunk() error = %v", err)
	}

	stop := []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	out, keep, err := processor.SanitizeChunk(stop, false)
	if err != nil {
		t.Fatalf("SanitizeChunk() error = %v", err)
	}
	if !keep {
		t.Fatal("the terminal chunk was dropped")
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls for a stream that recovered tool calls", got)
	}
}

func TestCodeBuddyStreamProcessorKeepsNativeCallsAlongsideMarkup(t *testing.T) {
	processor := NewCodeBuddyStreamProcessor()

	body := []byte(`{"choices":[{"index":0,"delta":{"content":"<invoke name=\"bash\"><parameter name=\"cmd\">pwd</parameter></invoke>","tool_calls":[{"index":0,"id":"call_native","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"stop"}]}`)
	out, keep, err := processor.SanitizeChunk(body, true)
	if err != nil {
		t.Fatalf("SanitizeChunk() error = %v", err)
	}
	if !keep {
		t.Fatal("a chunk with native tool calls was dropped")
	}
	calls := gjson.GetBytes(out, "choices.0.delta.tool_calls")
	if len(calls.Array()) != 1 {
		t.Fatalf("native tool calls were replaced: %s", calls.Raw)
	}
	if got := calls.Array()[0].Get("id").String(); got != "call_native" {
		t.Errorf("native tool call id = %q, want call_native", got)
	}
	if got := gjson.GetBytes(out, "choices.0.delta.content").String(); got != "" {
		t.Errorf("content = %q, want the markup removed", got)
	}
}

func TestCodeBuddyStreamProcessorKeepsUsageOnlyChunkIntact(t *testing.T) {
	processor := NewCodeBuddyStreamProcessor()

	markup := []byte(`{"choices":[{"index":0,"delta":{"content":"<invoke name=\"bash\"><parameter name=\"cmd\">pwd</parameter></invoke>"}}]}`)
	if _, _, err := processor.SanitizeChunk(markup, false); err != nil {
		t.Fatalf("SanitizeChunk() error = %v", err)
	}

	usage := []byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	out, keep, err := processor.SanitizeChunk(usage, true)
	if err != nil {
		t.Fatalf("SanitizeChunk() error = %v", err)
	}
	if !keep {
		t.Fatal("the usage chunk was dropped")
	}
	if got := gjson.GetBytes(out, "choices.0").Raw; got != "" {
		t.Errorf("a choice was fabricated in the usage-only chunk: %s", out)
	}
	if got := gjson.GetBytes(out, "usage.total_tokens").Int(); got != 3 {
		t.Errorf("usage.total_tokens = %d, want 3", got)
	}
}
func TestCodeBuddyStreamProcessorKeepsFencedMarkupAsText(t *testing.T) {
	processor := NewCodeBuddyStreamProcessor()
	body := []byte("{\"choices\":[{\"index\":0,\"delta\":{\"content\":\"```xml\\n<invoke name=\\\"bash\\\"><parameter name=\\\"cmd\\\">ls</parameter></invoke>\\n```\"}}]}")

	out, keep, err := processor.SanitizeChunk(body, true)
	if err != nil {
		t.Fatalf("SanitizeChunk() error = %v", err)
	}
	if !keep {
		t.Fatal("a fenced example chunk was dropped")
	}
	if gjson.GetBytes(out, "choices.0.delta.tool_calls").Exists() {
		t.Fatalf("markup inside a code fence must stay text: %s", out)
	}
	content := gjson.GetBytes(out, "choices.0.delta.content").String()
	if !strings.Contains(content, "<invoke") {
		t.Fatalf("fenced markup was stripped from the content: %q", content)
	}
}

func TestCodeBuddyStreamProcessorLeavesNativeToolCallsUntouched(t *testing.T) {
	processor := NewCodeBuddyStreamProcessor()
	body := []byte(`{"choices":[{"index":0,"delta":{"content":"done","tool_calls":[{"index":0,"id":"call_native","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":null}]}`)

	out, keep, err := processor.SanitizeChunk(body, true)
	if err != nil {
		t.Fatalf("SanitizeChunk() error = %v", err)
	}
	if !keep {
		t.Fatal("a native tool call chunk was dropped")
	}
	calls := gjson.GetBytes(out, "choices.0.delta.tool_calls")
	if !calls.IsArray() || len(calls.Array()) != 1 {
		t.Fatalf("native tool calls were modified: %s", calls.Raw)
	}
	if got := calls.Array()[0].Get("id").String(); got != "call_native" {
		t.Errorf("native tool call id = %q, want call_native", got)
	}
	if got := gjson.GetBytes(out, "choices.0.delta.content").String(); got != "done" {
		t.Errorf("content = %q, want done", got)
	}
}

func TestCodeBuddyStreamProcessorDropsEmptyChunks(t *testing.T) {
	processor := NewCodeBuddyStreamProcessor()
	body := []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":null}]}`)

	_, keep, err := processor.SanitizeChunk(body, false)
	if err != nil {
		t.Fatalf("SanitizeChunk() error = %v", err)
	}
	if keep {
		t.Fatal("an empty chunk must be dropped")
	}

	usageChunk := []byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	if _, keep, errUsage := processor.SanitizeChunk(usageChunk, false); errUsage != nil || !keep {
		t.Fatalf("usage chunk keep = %v, err = %v, want kept", keep, errUsage)
	}
}
func TestCodeBuddyStreamProcessorResidualChunkReleasesHeldBackText(t *testing.T) {
	processor := NewCodeBuddyStreamProcessor()

	// The visible text is followed by a '<' that looks like a streamed tool call
	// opener, so the processor holds the ambiguous tail back instead of emitting it.
	first := []byte(`{"id":"chatcmpl-9","model":"gpt-5","choices":[{"index":0,"delta":{"content":"final answer <tool_c"}}]}`)
	out, keep, err := processor.SanitizeChunk(first, false)
	if err != nil {
		t.Fatalf("SanitizeChunk() error = %v", err)
	}
	if !keep {
		t.Fatal("the chunk with visible text was dropped")
	}
	if got := gjson.GetBytes(out, "choices.0.delta.content").String(); got != "final answer " {
		t.Fatalf("content = %q, want the visible text without the ambiguous tail", got)
	}

	residual, err := processor.ResidualChunk()
	if err != nil {
		t.Fatalf("ResidualChunk() error = %v", err)
	}
	if len(residual) == 0 {
		t.Fatal("ResidualChunk() returned nothing for held-back markup")
	}
	if got := gjson.GetBytes(residual, "id").String(); got != "chatcmpl-9" {
		t.Errorf("residual chunk id = %q, want chatcmpl-9", got)
	}
	if got := gjson.GetBytes(residual, "model").String(); got != "gpt-5" {
		t.Errorf("residual chunk model = %q, want gpt-5", got)
	}

	// The tail never became markup, so it is released verbatim as plain text.
	if content := gjson.GetBytes(residual, "choices.0.delta.content").String(); content != "<tool_c" {
		t.Errorf("residual content = %q, want the released held-back tail", content)
	}
	if gjson.GetBytes(residual, "choices.0.delta.tool_calls").Exists() {
		t.Errorf("residual chunk invented a tool call: %s", residual)
	}
	if extra, errExtra := processor.ResidualChunk(); errExtra != nil || len(extra) != 0 {
		t.Fatalf("second ResidualChunk() = %s, err = %v, want empty", extra, errExtra)
	}
}

func TestCodeBuddyStreamProcessorResidualChunkFinishesMidStreamCall(t *testing.T) {
	processor := NewCodeBuddyStreamProcessor()

	// The inline call completes in a non-terminal chunk and upstream ends the stream
	// without any finish_reason, so the residual chunk must supply the only one.
	chunk := []byte(`{"id":"chatcmpl-7","model":"gpt-5","choices":[{"index":0,"delta":{"content":"<invoke name=\"bash\"><cmd>ls</cmd></invoke>"}}]}`)
	out, keep, err := processor.SanitizeChunk(chunk, false)
	if err != nil {
		t.Fatalf("SanitizeChunk() error = %v", err)
	}
	if !keep || gjson.GetBytes(out, "choices.0.delta.tool_calls.0.function.name").String() != "bash" {
		t.Fatalf("recovered chunk = %s, want an injected bash tool call", out)
	}
	if gjson.GetBytes(out, "choices.0.finish_reason").Exists() {
		t.Fatalf("non-terminal chunk got a finish_reason: %s", out)
	}

	residual, err := processor.ResidualChunk()
	if err != nil {
		t.Fatalf("ResidualChunk() error = %v", err)
	}
	if got := gjson.GetBytes(residual, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("residual finish_reason = %q (chunk %s), want tool_calls", got, residual)
	}
	if gjson.GetBytes(residual, "choices.0.delta.tool_calls").Exists() {
		t.Errorf("residual chunk repeated the tool call: %s", residual)
	}
	if extra, errExtra := processor.ResidualChunk(); errExtra != nil || len(extra) != 0 {
		t.Fatalf("second ResidualChunk() = %s, err = %v, want empty", extra, errExtra)
	}
}

func TestCodeBuddyStreamAggregatorRecoversMarkupForNonStreamClients(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-2","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"content":"listing now <tool_ca"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-2","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"content":"lls><invoke name=\"bash\"><parameter name=\"cmd\">ls</parameter></invoke></tool_calls>"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`,
		`data: [DONE]`,
	}, "\n")

	aggregated := CodeBuddyStreamAggregatorForSSE([]byte(stream), NewCodeBuddyStreamProcessor())
	if len(aggregated) == 0 {
		t.Fatal("CodeBuddyStreamAggregatorForSSE() returned no payload")
	}
	if got := gjson.GetBytes(aggregated, "choices.0.message.content").String(); got != "listing now " {
		t.Errorf("content = %q, want the text with the markup removed", got)
	}
	calls := gjson.GetBytes(aggregated, "choices.0.message.tool_calls")
	if !calls.IsArray() || len(calls.Array()) != 1 {
		t.Fatalf("tool_calls = %s, want one recovered call", calls.Raw)
	}
	if got := calls.Array()[0].Get("function.name").String(); got != "bash" {
		t.Errorf("tool name = %q, want bash", got)
	}
	if got := gjson.GetBytes(aggregated, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", got)
	}
	if got := gjson.GetBytes(aggregated, "usage.total_tokens").Int(); got != 7 {
		t.Errorf("usage.total_tokens = %d, want 7", got)
	}
}

// TestCodeBuddyStreamProcessorsAreIndependent runs several request-scoped
// processors at once: each must recover only its own tool calls.
func TestCodeBuddyStreamProcessorsAreIndependent(t *testing.T) {
	const workers = 8

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			processor := NewCodeBuddyStreamProcessor()
			name := fmt.Sprintf("cmd-%d", id)
			first := []byte(fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":"run <invoke name=\"%s\">"}}]}`, name))
			if _, _, err := processor.SanitizeChunk(first, false); err != nil {
				errs <- err
				return
			}
			second := []byte(fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":"<parameter name=\"cmd\">pwd</parameter></invoke>"}}]}`))
			out, keep, err := processor.SanitizeChunk(second, false)
			if err != nil {
				errs <- err
				return
			}
			if !keep {
				errs <- fmt.Errorf("worker %d: chunk dropped", id)
				return
			}
			calls := gjson.GetBytes(out, "choices.0.delta.tool_calls")
			if len(calls.Array()) != 1 || calls.Array()[0].Get("function.name").String() != name {
				errs <- fmt.Errorf("worker %d: tool calls = %s, want %s", id, calls.Raw, name)
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for errWorker := range errs {
		if errWorker != nil {
			t.Fatal(errWorker)
		}
	}
}

func TestCodeBuddyStreamAggregatorReturnsNilWithoutChoiceChunks(t *testing.T) {
	if got := CodeBuddyStreamAggregatorForSSE([]byte("data: [DONE]\n"), NewCodeBuddyStreamProcessor()); got != nil {
		t.Fatalf("CodeBuddyStreamAggregatorForSSE() = %v, want nil", got)
	}
}
