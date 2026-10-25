package helps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CodeBuddyChatPath is the OpenAI-compatible chat completions path served by the
// Tencent CodeBuddy global gateway.
const CodeBuddyChatPath = "/v2/chat/completions"

// CodeBuddySSEDataPrefix is the SSE field prefix prepended to every relayed chunk.
const CodeBuddySSEDataPrefix = "data: "

// CodeBuddyStreamProcessor post-processes one upstream CodeBuddy stream.
//
// The gateway sometimes streams tool calls as inline DSML text markup inside
// delta.content instead of native delta.tool_calls, so every content delta is
// routed through a DSML parser. Recovered tool calls are injected as native
// delta.tool_calls (never alongside existing native tool calls) with
// finish_reason "tool_calls", the markup is stripped from the emitted content,
// and chunks that end up empty are dropped. A processor is bound to a single
// request and is not safe for concurrent use.
type CodeBuddyStreamProcessor struct {
	parser  *CodeBuddyDSMLParser
	names   *CodeBuddyToolCallNameState
	nextIdx int64
	// sawFinishReason records that a terminal finish reason was already emitted, so
	// the residual chunk never adds a second one.
	sawFinishReason bool
	last            []byte
}

// codeBuddyTerminalFinishReason reports whether a choice carries a real finish
// reason. Null and empty values are not terminal: the gateway sends those on
// every intermediate delta.
func codeBuddyTerminalFinishReason(value gjson.Result) bool {
	return value.Exists() && value.Type == gjson.String && strings.TrimSpace(value.String()) != ""
}

// NewCodeBuddyStreamProcessor returns a processor for one request.
func NewCodeBuddyStreamProcessor() *CodeBuddyStreamProcessor {
	return &CodeBuddyStreamProcessor{
		parser: NewCodeBuddyDSMLParser(),
		names:  NewCodeBuddyToolCallNameState(),
	}
}

// SanitizeChunk post-processes one decoded upstream chunk. It leaves non-JSON
// payloads and payloads without a content delta untouched apart from the usual
// chunk sanitation. includeResidual is used for the final chunk of a stream and
// additionally releases the parser's held-back tail.
//
// The returned keep flag reports whether the chunk carries anything worth
// forwarding; a dropped chunk must not be relayed to the client.
func (p *CodeBuddyStreamProcessor) SanitizeChunk(chunk []byte, includeResidual bool) ([]byte, bool, error) {
	if len(chunk) == 0 || !gjson.ValidBytes(chunk) {
		return chunk, true, nil
	}
	if p == nil {
		return chunk, true, nil
	}

	out := bytes.Clone(chunk)
	terminalIndex := -1
	choices := gjson.GetBytes(out, "choices")
	if choices.IsArray() {
		for i, choice := range choices.Array() {
			// Keep synthesized tool call indexes after any native one.
			for _, native := range choice.Get("delta.tool_calls").Array() {
				p.reserveToolCallIndex(native.Get("index").Int())
			}

			// A choice is terminal when upstream reported a finish reason for it or the
			// caller flagged the chunk as the last of the stream.
			terminal := includeResidual || codeBuddyTerminalFinishReason(choice.Get("finish_reason"))

			content := choice.Get("delta.content")
			hasContent := content.Exists() && content.Type == gjson.String
			if !hasContent && !terminal {
				continue
			}

			text := ""
			if hasContent {
				text = content.String()
			}
			text, calls := p.parser.Feed(text)
			if terminal {
				// Release everything the parser still holds on the choice that ends this
				// stream, so nothing is emitted after its finish_reason.
				residual, more := p.parser.Flush()
				text += residual
				calls = append(calls, more...)
				terminalIndex = i
			}

			// Only rewrite the payload when the parser changed something, so a plain
			// text stream keeps its original bytes.

			if text != "" || (hasContent && text != content.String()) {
				updated, err := sjson.SetBytes(out, fmt.Sprintf("choices.%d.delta.content", i), text)
				if err != nil {
					return nil, false, fmt.Errorf("codebuddy: rewrite content delta: %w", err)
				}
				out = updated
			}

			// Native tool calls win: recovered calls are only injected when the chunk
			// carries none, and they are always appended after the native indexes.
			if len(calls) > 0 && len(choice.Get("delta.tool_calls").Array()) == 0 {
				updated, err := p.injectToolCalls(out, i, calls)
				if err != nil {
					return nil, false, err
				}
				out = updated
			}
		}
	}

	sanitized, err := SanitizeCodeBuddyChunk(out, p.names)
	if err != nil {
		return nil, false, err
	}

	if terminalIndex >= 0 {
		// A stream that recovered tool calls must not end as a normal stop, and a
		// terminal choice without a finish reason still needs its single one. An
		// explicit length/error finish reason is preserved.
		reason := strings.TrimSpace(gjson.GetBytes(sanitized, fmt.Sprintf("choices.%d.finish_reason", terminalIndex)).String())
		if reason == "" || reason == "stop" {
			if p.parser.Emitted() {
				updated, errFinish := sjson.SetBytes(sanitized, fmt.Sprintf("choices.%d.finish_reason", terminalIndex), "tool_calls")
				if errFinish != nil {
					return nil, false, fmt.Errorf("codebuddy: set tool_calls finish reason: %w", errFinish)
				}
				sanitized = updated
				reason = "tool_calls"
			}
		}
		if reason != "" {
			p.sawFinishReason = true
		}
	}

	if !codeBuddyChunkHasContent(sanitized) {
		return nil, false, nil
	}
	p.rememberShape(sanitized)
	return sanitized, true, nil
}

// ResidualChunk releases the parser's held-back tail at the end of a stream. It
// returns nil when nothing is left, or a synthesized chunk shaped like the last
// upstream chunk.
func (p *CodeBuddyStreamProcessor) ResidualChunk() ([]byte, error) {
	if p == nil {
		return nil, nil
	}
	text, calls := p.parser.Flush()
	// A call recovered mid-stream still needs a terminal finish_reason when
	// upstream ended without sending one.
	needFinish := p.parser.Emitted() && !p.sawFinishReason
	if text == "" && len(calls) == 0 && !needFinish {
		return nil, nil
	}

	chunk := p.newShapeChunk()
	delta := map[string]any{}
	if text != "" {
		delta["content"] = text
	}
	if len(calls) > 0 {
		toolCalls, err := p.toolCallPayloads(calls)
		if err != nil {
			return nil, err
		}
		delta["tool_calls"] = toolCalls
	}

	choice := map[string]any{"index": 0, "delta": delta}
	if len(calls) > 0 || needFinish {
		choice["finish_reason"] = "tool_calls"
		p.sawFinishReason = true
	}
	encoded, err := json.Marshal([]map[string]any{choice})
	if err != nil {
		return nil, fmt.Errorf("codebuddy: encode residual chunk: %w", err)
	}
	out, err := sjson.SetRawBytes(chunk, "choices", encoded)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: build residual chunk: %w", err)
	}
	p.rememberShape(out)
	return out, nil
}

// injectToolCalls adds recovered tool calls as native deltas, keeping them after
// every native index seen so far.
func (p *CodeBuddyStreamProcessor) injectToolCalls(chunk []byte, choiceIndex int, calls []CodeBuddyDSMLToolCall) ([]byte, error) {
	toolCalls, err := p.toolCallPayloads(calls)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(toolCalls)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: encode recovered tool calls: %w", err)
	}
	out, err := sjson.SetRawBytes(chunk, fmt.Sprintf("choices.%d.delta.tool_calls", choiceIndex), encoded)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: inject recovered tool calls: %w", err)
	}
	return out, nil
}

func (p *CodeBuddyStreamProcessor) toolCallPayloads(calls []CodeBuddyDSMLToolCall) ([]map[string]any, error) {
	payloads := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		index := p.nextIdx
		p.nextIdx++
		arguments := call.Arguments
		if arguments == "" {
			arguments = "{}"
		}
		payloads = append(payloads, map[string]any{
			"index": index,
			"id":    call.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": arguments,
			},
		})
	}
	return payloads, nil
}

// reserveToolCallIndex keeps synthesized tool call indexes after any native one.
func (p *CodeBuddyStreamProcessor) reserveToolCallIndex(index int64) {
	if index >= p.nextIdx {
		p.nextIdx = index + 1
	}
}

func (p *CodeBuddyStreamProcessor) rememberShape(chunk []byte) {
	p.last = bytes.Clone(chunk)
	for _, choice := range gjson.GetBytes(chunk, "choices").Array() {
		for _, toolCall := range choice.Get("delta.tool_calls").Array() {
			p.reserveToolCallIndex(toolCall.Get("index").Int())
		}
	}
}

// newShapeChunk copies the envelope fields of the last upstream chunk so a
// synthesized chunk stays compatible with the client's translator.
func (p *CodeBuddyStreamProcessor) newShapeChunk() []byte {
	chunk := map[string]any{
		"id":      "chatcmpl-codebuddy",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
	}
	if len(p.last) > 0 {
		for _, field := range []string{"id", "object", "created", "model"} {
			if value := gjson.GetBytes(p.last, field); value.Exists() {
				chunk[field] = value.Value()
			}
		}
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		return []byte(`{"id":"chatcmpl-codebuddy","object":"chat.completion.chunk","created":0,"choices":[]}`)
	}
	return encoded
}

// codeBuddyChunkHasContent reports whether a chunk still carries something worth
// forwarding: client-visible delta fields, a finish reason, or usage.
func codeBuddyChunkHasContent(chunk []byte) bool {
	if !gjson.ValidBytes(chunk) {
		return true
	}
	if gjson.GetBytes(chunk, "usage").Exists() {
		return true
	}
	for _, choice := range gjson.GetBytes(chunk, "choices").Array() {
		if strings.TrimSpace(choice.Get("finish_reason").String()) != "" {
			return true
		}
		delta := choice.Get("delta")
		if !delta.Exists() {
			continue
		}
		if gjson.GetBytes([]byte(delta.Raw), "tool_calls").Exists() || gjson.GetBytes([]byte(delta.Raw), "function_call").Exists() {
			return true
		}
		for _, field := range []string{"content", "reasoning_content", "reasoning", "refusal", "audio", "images"} {
			value := delta.Get(field)
			if !value.Exists() || value.Type == gjson.Null {
				continue
			}
			if value.Type == gjson.String && value.String() == "" {
				continue
			}
			return true
		}
	}
	return false
}

// PrepareCodeBuddyRequestBody applies the built-in CodeBuddy payload transforms.
//
// The global gateway always answers with an SSE stream, so "stream" is forced to
// true regardless of the client's request; non-streaming clients are served by
// aggregating the SSE afterwards. stream_options.include_usage is enabled so the
// final chunk carries usage. tool_choice is flattened from the OpenAI object
// form to the string form the gateway expects.
//
// These are built-in normalizations and therefore run before payload config,
// which stays the last word on the business payload.
func PrepareCodeBuddyRequestBody(body []byte) ([]byte, error) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, nil
	}

	out, err := sjson.SetBytes(body, "stream", true)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: force stream: %w", err)
	}

	if includeUsage := gjson.GetBytes(out, "stream_options.include_usage"); !includeUsage.Exists() {
		out, err = sjson.SetBytes(out, "stream_options.include_usage", true)
		if err != nil {
			return nil, fmt.Errorf("codebuddy: set stream_options.include_usage: %w", err)
		}
	}

	normalized, err := normalizeCodeBuddyToolChoice(out)
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

// normalizeCodeBuddyToolChoice flattens object tool_choice values into the
// string form accepted by the CodeBuddy gateway:
//
//	{"type":"function","function":{"name":"X"}} -> "X"
//	{"type":"function"}                         -> "required"
//	"none"/"auto"/"required"/model name         -> unchanged
func normalizeCodeBuddyToolChoice(body []byte) ([]byte, error) {
	choice := gjson.GetBytes(body, "tool_choice")
	if !choice.Exists() || !choice.IsObject() {
		return body, nil
	}

	value := ""
	switch strings.ToLower(strings.TrimSpace(choice.Get("type").String())) {
	case "function":
		name := strings.TrimSpace(choice.Get("function.name").String())
		if name == "" {
			value = "required"
		} else {
			value = name
		}
	case "required":
		value = "required"
	case "auto":
		value = "auto"
	case "none":
		value = "none"
	default:
		value = strings.TrimSpace(choice.Get("type").String())
	}
	if value == "" {
		return body, nil
	}

	out, err := sjson.SetBytes(body, "tool_choice", value)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: normalize tool_choice: %w", err)
	}
	return out, nil
}

// CodeBuddyConversationSeed derives the X-Conversation-ID seed from a request
// body. The gateway expects the first message's content; objects (content part
// arrays) are encoded as compact JSON with sorted keys, and an empty or
// unparseable body falls back to the whole body so the seed is never empty.
func CodeBuddyConversationSeed(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	first := gjson.GetBytes(payload, "messages.0.content")
	if first.Exists() {
		if first.Type == gjson.String {
			if seed := first.String(); strings.TrimSpace(seed) != "" {
				return seed
			}
		} else {
			if raw := first.Raw; raw != "" && raw != "null" {
				if compact, err := compactJSONWithSortedKeys([]byte(raw)); err == nil && compact != "" {
					return compact
				}
				return raw
			}
		}
	}
	return string(payload)
}

// compactJSONWithSortedKeys re-encodes JSON with map keys sorted so the same
// content always yields the same conversation seed on every request.
func compactJSONWithSortedKeys(raw []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return "", err
	}
	var buffer bytes.Buffer
	if err := writeSortedJSON(&buffer, decoded); err != nil {
		return "", err
	}
	return buffer.String(), nil
}

func writeSortedJSON(buffer *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buffer.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				buffer.WriteByte(',')
			}
			encodedKey, err := json.Marshal(key)
			if err != nil {
				return err
			}
			buffer.Write(encodedKey)
			buffer.WriteByte(':')
			if err = writeSortedJSON(buffer, typed[key]); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
	case []any:
		buffer.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				buffer.WriteByte(',')
			}
			if err := writeSortedJSON(buffer, item); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		buffer.Write(encoded)
	}
	return nil
}

// CodeBuddyToolCallNameState tracks the last non-empty tool call name seen for
// each tool call index. The gateway sends the name only in the first delta of a
// tool call, and some later deltas repeat the index with an empty name.
type CodeBuddyToolCallNameState struct {
	names map[int64]string
}

// NewCodeBuddyToolCallNameState creates an empty tool call name tracker.
func NewCodeBuddyToolCallNameState() *CodeBuddyToolCallNameState {
	return &CodeBuddyToolCallNameState{names: make(map[int64]string)}
}

// Record stores the name for a tool call index when it is non-empty.
func (s *CodeBuddyToolCallNameState) Record(index int64, name string) {
	if s == nil || strings.TrimSpace(name) == "" {
		return
	}
	if s.names == nil {
		s.names = make(map[int64]string)
	}
	s.names[index] = name
}

// Lookup returns the last non-empty name recorded for a tool call index.
func (s *CodeBuddyToolCallNameState) Lookup(index int64) string {
	if s == nil || s.names == nil {
		return ""
	}
	return s.names[index]
}

// SanitizeCodeBuddyChunk removes gateway quirks from one decoded upstream chunk:
// empty or null finish_reason values are deleted, and an empty tool call
// function name is backfilled from the name previously seen for that index.
// Chunks that are not valid JSON are returned unchanged.
func SanitizeCodeBuddyChunk(chunk []byte, state *CodeBuddyToolCallNameState) ([]byte, error) {
	if len(chunk) == 0 || !gjson.ValidBytes(chunk) {
		return chunk, nil
	}

	out := chunk
	choices := gjson.GetBytes(out, "choices")
	if !choices.IsArray() {
		return out, nil
	}

	for i, choice := range choices.Array() {
		finish := choice.Get("finish_reason")
		if finish.Exists() && (finish.Type == gjson.Null || strings.TrimSpace(finish.String()) == "") {
			path := fmt.Sprintf("choices.%d.finish_reason", i)
			updated, err := sjson.DeleteBytes(out, path)
			if err != nil {
				return nil, fmt.Errorf("codebuddy: delete empty finish_reason: %w", err)
			}
			out = updated
		}

		for j, toolCall := range choice.Get("delta.tool_calls").Array() {
			index := toolCall.Get("index").Int()
			name := toolCall.Get("function.name").String()
			if strings.TrimSpace(name) == "" {
				if backfill := state.Lookup(index); backfill != "" {
					path := fmt.Sprintf("choices.%d.delta.tool_calls.%d.function.name", i, j)
					updated, err := sjson.SetBytes(out, path, backfill)
					if err != nil {
						return nil, fmt.Errorf("codebuddy: backfill tool call name: %w", err)
					}
					out = updated
				}
				continue
			}
			state.Record(index, name)
		}
	}
	return out, nil
}

// CodeBuddyChunkDataLine extracts the JSON payload of one SSE line. It reports
// false for blank lines, comments, and the terminal "[DONE]" sentinel.
func CodeBuddyChunkDataLine(line []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] == ':' {
		return nil, false
	}
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil, false
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil, false
	}
	return payload, true
}

// CodeBuddyStreamAggregator folds an OpenAI-compatible SSE stream
// (chat.completion.chunk) into one non-streaming chat.completion payload.
//
// Content and reasoning_content are concatenated, tool calls are merged by
// index with arguments concatenated, the last usage object and the last
// non-empty finish_reason win, and tool calls without a name are dropped.
type CodeBuddyStreamAggregator struct {
	base    []byte
	choices map[int64]*codeBuddyChoiceAccumulator
	usage   []byte
	order   []int64
	names   *CodeBuddyToolCallNameState
}

type codeBuddyChoiceAccumulator struct {
	index            int64
	role             string
	content          string
	reasoningContent string
	finishReason     string
	toolCalls        map[int64]*codeBuddyToolCallAccumulator
	toolCallOrder    []int64
	seen             bool
}

type codeBuddyToolCallAccumulator struct {
	id        string
	name      string
	arguments string
	callType  string
}

// NewCodeBuddyStreamAggregator creates an empty aggregator.
func NewCodeBuddyStreamAggregator() *CodeBuddyStreamAggregator {
	return &CodeBuddyStreamAggregator{
		choices: make(map[int64]*codeBuddyChoiceAccumulator),
		names:   NewCodeBuddyToolCallNameState(),
	}
}

// AddChunk folds one SSE data payload into the aggregate. Non-JSON payloads and
// payloads without choices are ignored.
func (a *CodeBuddyStreamAggregator) AddChunk(payload []byte) error {
	if a == nil || len(payload) == 0 || !gjson.ValidBytes(payload) {
		return nil
	}

	if a.base == nil {
		a.base = bytes.Clone(payload)
	}
	if usage := gjson.GetBytes(payload, "usage"); usage.Exists() && usage.IsObject() {
		a.usage = []byte(usage.Raw)
	}

	for i, choice := range gjson.GetBytes(payload, "choices").Array() {
		index := choice.Get("index").Int()
		if !choice.Get("index").Exists() {
			index = int64(i)
		}
		accumulator := a.choice(index)
		accumulator.seen = true

		if role := choice.Get("delta.role").String(); role != "" {
			accumulator.role = role
		}
		accumulator.content += choice.Get("delta.content").String()
		accumulator.reasoningContent += choice.Get("delta.reasoning_content").String()

		finish := choice.Get("finish_reason")
		if finish.Exists() && finish.Type != gjson.Null && strings.TrimSpace(finish.String()) != "" {
			accumulator.finishReason = finish.String()
		}

		for j, toolCall := range choice.Get("delta.tool_calls").Array() {
			toolCallIndex := toolCall.Get("index").Int()
			if !toolCall.Get("index").Exists() {
				toolCallIndex = int64(j)
			}
			entry := accumulator.toolCall(toolCallIndex)
			if id := toolCall.Get("id").String(); strings.TrimSpace(id) != "" {
				entry.id = id
			}
			if callType := toolCall.Get("type").String(); strings.TrimSpace(callType) != "" {
				entry.callType = callType
			}
			name := toolCall.Get("function.name").String()
			if strings.TrimSpace(name) != "" {
				entry.name = name
				a.names.Record(toolCallIndex, name)
			}
			entry.arguments += toolCall.Get("function.arguments").String()
		}
	}
	return nil
}

func (a *CodeBuddyStreamAggregator) choice(index int64) *codeBuddyChoiceAccumulator {
	accumulator, ok := a.choices[index]
	if !ok {
		accumulator = &codeBuddyChoiceAccumulator{
			index:     index,
			toolCalls: make(map[int64]*codeBuddyToolCallAccumulator),
		}
		a.choices[index] = accumulator
		a.order = append(a.order, index)
	}
	return accumulator
}

func (c *codeBuddyChoiceAccumulator) toolCall(index int64) *codeBuddyToolCallAccumulator {
	entry, ok := c.toolCalls[index]
	if !ok {
		entry = &codeBuddyToolCallAccumulator{}
		c.toolCalls[index] = entry
		c.toolCallOrder = append(c.toolCallOrder, index)
	}
	return entry
}

// Payload renders the aggregated chat.completion JSON. It returns nil when no
// chunk with choices was observed.
func (a *CodeBuddyStreamAggregator) Payload() ([]byte, error) {
	if a == nil || len(a.order) == 0 {
		return nil, nil
	}

	out := a.base
	if out == nil {
		out = []byte(`{"object":"chat.completion","choices":[]}`)
	}

	choices := make([]map[string]any, 0, len(a.order))
	for _, index := range a.order {
		accumulator := a.choices[index]
		if accumulator == nil || !accumulator.seen {
			continue
		}

		message := map[string]any{}
		role := accumulator.role
		if role == "" {
			role = "assistant"
		}
		message["role"] = role
		message["content"] = accumulator.content
		if accumulator.reasoningContent != "" {
			message["reasoning_content"] = accumulator.reasoningContent
		}

		toolCalls := make([]map[string]any, 0, len(accumulator.toolCallOrder))
		for _, toolIndex := range accumulator.toolCallOrder {
			entry := accumulator.toolCalls[toolIndex]
			if entry == nil || strings.TrimSpace(entry.name) == "" {
				// Tool calls without a name cannot be replayed to the client.
				continue
			}
			callType := entry.callType
			if callType == "" {
				callType = "function"
			}
			toolCalls = append(toolCalls, map[string]any{
				"index": toolIndex,
				"id":    entry.id,
				"type":  callType,
				"function": map[string]any{
					"name":      entry.name,
					"arguments": entry.arguments,
				},
			})
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}

		choice := map[string]any{
			"index":   accumulator.index,
			"message": message,
		}
		if accumulator.finishReason != "" {
			choice["finish_reason"] = accumulator.finishReason
		} else {
			choice["finish_reason"] = "stop"
		}
		choices = append(choices, choice)
	}
	if len(choices) == 0 {
		return nil, nil
	}

	encoded, err := json.Marshal(choices)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: encode aggregated choices: %w", err)
	}
	out, err = sjson.SetRawBytes(out, "choices", encoded)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: set aggregated choices: %w", err)
	}
	if a.usage != nil {
		out, err = sjson.SetRawBytes(out, "usage", a.usage)
		if err != nil {
			return nil, fmt.Errorf("codebuddy: set aggregated usage: %w", err)
		}
	}
	out, err = sjson.SetBytes(out, "object", "chat.completion")
	if err != nil {
		return nil, fmt.Errorf("codebuddy: set aggregated object: %w", err)
	}
	return out, nil
}

// CodeBuddyStreamAggregatorForSSE folds a complete upstream SSE body into one
// chat.completion payload using the request's stream processor, so recovered
// inline tool calls and cleaned content reach non-streaming clients. It returns
// nil when the stream carried no content chunks.
func CodeBuddyStreamAggregatorForSSE(body []byte, processor *CodeBuddyStreamProcessor) []byte {
	aggregator := NewCodeBuddyStreamAggregator()
	lines := bytes.Split(body, []byte("\n"))
	for i, rawLine := range lines {
		payload, ok := CodeBuddyChunkDataLine(rawLine)
		if !ok {
			continue
		}
		processed := payload
		if processor != nil {
			// The last data line of the body releases any held-back markup tail.
			includeResidual := !codeBuddyHasLaterDataLine(lines, i)
			updated, keep, err := processor.SanitizeChunk(payload, includeResidual)
			if err != nil {
				return nil
			}
			if !keep {
				continue
			}
			processed = updated
		}
		if err := aggregator.AddChunk(processed); err != nil {
			return nil
		}
	}
	if processor != nil {
		if residual, err := processor.ResidualChunk(); err == nil && len(residual) > 0 {
			if errAdd := aggregator.AddChunk(residual); errAdd != nil {
				return nil
			}
		}
	}
	aggregated, err := aggregator.Payload()
	if err != nil {
		return nil
	}
	return aggregated
}

// codeBuddyHasLaterDataLine reports whether another "data:" line follows index.
func codeBuddyHasLaterDataLine(lines [][]byte, index int) bool {
	for i := index + 1; i < len(lines); i++ {
		if _, ok := CodeBuddyChunkDataLine(lines[i]); ok {
			return true
		}
	}
	return false
}
