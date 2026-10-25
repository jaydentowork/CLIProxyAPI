package helps

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"html"
	"regexp"
	"strings"
	"unicode"
)

// CodeBuddyDSMLToolCall is a tool call recovered from inline text markup: the
// CodeBuddy upstream sometimes streams tool calls as DeepSeek "DSML" style text
// instead of native delta.tool_calls.
type CodeBuddyDSMLToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON object string built from the "<invoke>" parameters
}

// CodeBuddyDSMLParser recovers those inline tool calls while a stream is being
// pumped to the client.
//
// It is a port of the Python reference parser (workbuddy2api,
// codebuddy_proxy/dsml_parser.py). Feed returns the text that is safe to emit
// with the markup removed plus the tool calls completed by that chunk, holding
// back markup that is still incomplete; Flush ends the stream. A parser is bound to
// a single stream and is not safe for concurrent use.
type CodeBuddyDSMLParser struct {
	buffer []rune
	// bufferStartsLine reports whether buffer[0] sits at the start of a line. After
	// a chunk is released or a block is consumed the buffer may begin mid-line, and
	// only a fence marker at the start of a line opens a fenced code block.
	bufferStartsLine bool
	emitted          bool
}

var (
	// codeBuddyBlockNames are the block level elements the stream buffer looks
	// for, in the order the reference parser scans them: a wrapped <tool_calls>
	// block wins over a standalone <invoke>.
	codeBuddyBlockNames = []string{"tool_calls", "invoke"}

	// codeBuddyDSMLVariants are the accepted spellings of the DSML marker.
	codeBuddyDSMLVariants = [][]rune{
		[]rune("｜｜DSML｜｜"),
		[]rune("||DSML||"),
		[]rune("|DSML|"),
	}

	// codeBuddyToolMarkupNames maps a raw tag name to its canonical name.
	// dsmlOnly names are only recognised when they carry a DSML marker prefix.
	codeBuddyToolMarkupNames = []struct {
		raw       string
		canonical string
		dsmlOnly  bool
	}{
		{"tool_calls", "tool_calls", false},
		{"tool-calls", "tool_calls", true},
		{"toolcalls", "tool_calls", true},
		{"invoke", "invoke", false},
		{"parameter", "parameter", false},
	}

	// codeBuddyToolCallOpeners are the tag name prefixes that make a held-back
	// '<' look like the start of a streamed tool call block.
	codeBuddyToolCallOpeners = []string{
		"tool", "invoke", "tool_calls", "tool-calls", "toolcalls", "tool_call", "tool-call",
	}

	codeBuddyFenceMarkers = [][]rune{[]rune("```"), []rune("~~~")}

	codeBuddyCDATAStart   = []rune("<![CDATA[")
	codeBuddyCDATAEnd     = []rune("]]>")
	codeBuddyCommentStart = []rune("<!--")
	codeBuddyCommentEnd   = []rune("-->")
	codeBuddyPIStart      = []rune("<?")
	codeBuddyPIEnd        = []rune("?>")
	codeBuddyNewline      = []rune{'\n'}

	// codeBuddyIgnoredSectionOpeners are the markup starts whose content the scanner
	// skips; a tail that may still become one of them must stay buffered.
	codeBuddyIgnoredSectionOpeners = [][]rune{codeBuddyCDATAStart, codeBuddyCommentStart, codeBuddyPIStart}
)

var codeBuddyXMLAttributePattern = regexp.MustCompile(`(?i)([a-z0-9_:-]+)\s*=\s*["']([^"']*)["']`)

// NewCodeBuddyDSMLParser returns a parser for one CodeBuddy stream.
func NewCodeBuddyDSMLParser() *CodeBuddyDSMLParser {
	return &CodeBuddyDSMLParser{bufferStartsLine: true} // a stream starts at column 0
}

// Feed consumes the next chunk of upstream content and returns the text that may
// be emitted (tool call markup removed, partial markup held back) together with
// the tool calls that this chunk completed.
func (p *CodeBuddyDSMLParser) Feed(delta string) (string, []CodeBuddyDSMLToolCall) {
	if delta != "" {
		p.buffer = append(p.buffer, []rune(delta)...)
	}

	// Wrapped <tool_calls> blocks are consumed before standalone <invoke> blocks,
	// mirroring the reference parser's two sequential passes over the buffer.
	for _, name := range codeBuddyBlockNames {
		text, calls, stop := p.takeBlock(name, false)
		if stop {
			return text, calls
		}
	}

	holdFrom := codeBuddyHoldFrom(p.buffer, p.bufferStartsLine)
	text := string(p.buffer[:holdFrom])
	p.release(holdFrom)
	return text, nil
}

// release drops the first n runes of the buffer and remembers whether what is left
// still sits at the start of a line.
func (p *CodeBuddyDSMLParser) release(n int) {
	if n > 0 {
		p.bufferStartsLine = p.buffer[n-1] == '\n'
	}
	p.buffer = p.buffer[n:]
}

// Flush ends the stream: it consumes every block that is still complete in the
// buffer and returns everything else as text. Whatever is left is incomplete or
// not parseable markup, so it reaches the client rather than being dropped.
func (p *CodeBuddyDSMLParser) Flush() (string, []CodeBuddyDSMLToolCall) {
	var text strings.Builder
	var calls []CodeBuddyDSMLToolCall
	for {
		chunk, detected, consumed := p.flushBlock()
		text.WriteString(chunk)
		calls = append(calls, detected...)
		if !consumed {
			break
		}
	}
	leftover := string(p.buffer)
	p.release(len(p.buffer))
	text.WriteString(leftover)
	return text.String(), calls
}

// flushBlock consumes the next block that is still complete in the buffer, or
// releases the text that may be emitted, and reports whether it consumed a block.
func (p *CodeBuddyDSMLParser) flushBlock() (string, []CodeBuddyDSMLToolCall, bool) {
	for _, name := range codeBuddyBlockNames {
		text, calls, stop := p.takeBlock(name, true)
		if stop {
			return text, calls, true
		}
	}
	holdFrom := codeBuddyHoldFrom(p.buffer, p.bufferStartsLine)
	text := string(p.buffer[:holdFrom])
	p.release(holdFrom)
	return text, nil, false
}

// Emitted reports whether any tool call has been produced so far.
func (p *CodeBuddyDSMLParser) Emitted() bool {
	return p.emitted
}

// CodeBuddyParseDSML parses a complete (non-streamed) response text in one shot.
// It returns the text with the tool call markup removed and every tool call in
// document order.
func CodeBuddyParseDSML(full string) (string, []CodeBuddyDSMLToolCall) {
	parser := NewCodeBuddyDSMLParser()
	text, calls := parser.Feed(full)
	residual, detected := parser.Flush()
	return text + residual, append(calls, detected...)
}

// takeBlock consumes the first complete element named name. stop reports that the
// caller must not look for any other element in this call: a block was consumed, or
// its closing tag has not arrived yet. While flushing, a tag that can never be
// closed any more is skipped so that a later complete block is still recognised.
func (p *CodeBuddyDSMLParser) takeBlock(name string, flushing bool) (string, []CodeBuddyDSMLToolCall, bool) {
	for i := 0; i < len(p.buffer); {
		tag, found := codeBuddyFindToolMarkupTagOutsideIgnored(p.buffer, i, p.bufferStartsLine)
		if !found {
			break
		}
		if tag.name == name && !tag.closing {
			closeTag, closed := codeBuddyFindMatchingToolMarkupClose(p.buffer, tag)
			if !closed {
				if !flushing {
					return "", nil, true
				}
				i = tag.end + 1
				continue
			}
			block := p.buffer[tag.start : closeTag.end+1]
			if calls := codeBuddyParseToolCalls(block); len(calls) > 0 {
				p.emitted = true
				text := string(p.buffer[:tag.start])
				p.release(closeTag.end + 1)
				return text, calls, true
			}
		}
		i = tag.end + 1
	}
	return "", nil, false
}

// codeBuddyToolMarkupTag is a scanned tag: end is the index of its closing '>'.
type codeBuddyToolMarkupTag struct {
	start      int
	end        int
	name       string
	closing    bool
	attributes string
}

// codeBuddyParseToolCalls finds every <invoke> element in text and converts it to
// a tool call. Elements without a name attribute are skipped.
func codeBuddyParseToolCalls(text []rune) []CodeBuddyDSMLToolCall {
	var calls []CodeBuddyDSMLToolCall
	for i := 0; i < len(text); {
		tag, found := codeBuddyFindToolMarkupTagOutsideIgnored(text, i, false)
		if !found {
			break
		}
		if tag.name == "invoke" && !tag.closing {
			if closeTag, closed := codeBuddyFindMatchingToolMarkupClose(text, tag); closed {
				call, ok := codeBuddyParseInvokeElement(tag.attributes, string(text[tag.end+1:closeTag.start]))
				if ok {
					calls = append(calls, call)
				}
				i = closeTag.end + 1
				continue
			}
		}
		i = tag.end + 1
	}
	return calls
}

// codeBuddyParseInvokeElement builds a tool call from an <invoke> element.
func codeBuddyParseInvokeElement(attrsText, body string) (CodeBuddyDSMLToolCall, bool) {
	toolName := codeBuddyParseXMLAttributes(attrsText)["name"]
	if toolName == "" {
		return CodeBuddyDSMLToolCall{}, false
	}
	return CodeBuddyDSMLToolCall{
		ID:        codeBuddyNewToolCallID(),
		Name:      toolName,
		Arguments: codeBuddyMarshalArguments(codeBuddyParseInvokeParameters(body)),
	}, true
}

// codeBuddyParseInvokeParameters recursively parses the parameters inside an
// <invoke> body: <parameter name="x">value</parameter> (which wins on duplicate
// names), any other element <x>value</x>, CDATA blocks (never HTML decoded) and
// nested element groups.
func codeBuddyParseInvokeParameters(body string) map[string]any {
	text := []rune(body)
	params := map[string]any{}

	for i := 0; i < len(text); {
		for i < len(text) && codeBuddyIsXMLSpace(text[i]) {
			i++
		}
		if i >= len(text) {
			break
		}
		tagStart := codeBuddyIndexRunes(text, i, []rune{'<'})
		if tagStart == -1 {
			break
		}
		tag, found := codeBuddyScanToolMarkupTagAt(text, tagStart)
		if !found || tag.closing {
			i = tagStart + 1
			continue
		}
		closeTag, closed := codeBuddyFindMatchingToolMarkupClose(text, tag)
		if !closed {
			i = tag.end + 1
			continue
		}
		valueText := strings.TrimSpace(string(text[tag.end+1 : closeTag.start]))

		if tag.name == "parameter" {
			if paramName := codeBuddyParseXMLAttributes(tag.attributes)["name"]; paramName != "" {
				params[paramName] = codeBuddyParameterValue(valueText)
			}
		} else if paramName := tag.name; paramName != "" {
			if _, exists := params[paramName]; !exists {
				if raw, ok := codeBuddyCDATAContent(valueText); ok {
					params[paramName] = raw
					i = closeTag.end + 1
					continue
				}
				if nested := codeBuddyNestedParameters(valueText); nested != nil {
					params[paramName] = nested
				} else {
					params[paramName] = html.UnescapeString(valueText)
				}
			}
		}
		i = closeTag.end + 1
	}
	return params
}

// codeBuddyParameterValue resolves a <parameter> value: a full CDATA block keeps
// its content verbatim, anything else is HTML unescaped.
func codeBuddyParameterValue(valueText string) any {
	if raw, ok := codeBuddyCDATAContent(valueText); ok {
		return raw
	}
	return html.UnescapeString(valueText)
}

// codeBuddyCDATAContent returns the content of a value that is a complete CDATA
// block.
func codeBuddyCDATAContent(valueText string) (string, bool) {
	value := []rune(valueText)
	if len(value) < len(codeBuddyCDATAStart)+len(codeBuddyCDATAEnd) {
		return "", false
	}
	if !codeBuddyHasRunePrefix(value, 0, codeBuddyCDATAStart) {
		return "", false
	}
	if !codeBuddyHasRunePrefix(value, len(value)-len(codeBuddyCDATAEnd), codeBuddyCDATAEnd) {
		return "", false
	}
	return string(value[len(codeBuddyCDATAStart) : len(value)-len(codeBuddyCDATAEnd)]), true
}

// codeBuddyNestedParameters parses valueText as a group of nested parameters,
// returning nil when it does not hold a complete element.
func codeBuddyNestedParameters(valueText string) map[string]any {
	if !strings.ContainsRune(valueText, '<') || !strings.ContainsRune(valueText, '>') {
		return nil
	}
	value := []rune(valueText)
	tagStart := codeBuddyIndexRunes(value, 0, []rune{'<'})
	if tagStart == -1 {
		return nil
	}
	tag, found := codeBuddyScanToolMarkupTagAt(value, tagStart)
	if !found || tag.closing {
		return nil
	}
	if _, closed := codeBuddyFindMatchingToolMarkupClose(value, tag); !closed {
		return nil
	}
	nested := codeBuddyParseInvokeParameters(valueText)
	if len(nested) == 0 {
		return nil
	}
	return nested
}

// codeBuddyParseXMLAttributes parses name="value" attributes and HTML unescapes
// their values.
func codeBuddyParseXMLAttributes(attrsText string) map[string]string {
	attrs := map[string]string{}
	for _, match := range codeBuddyXMLAttributePattern.FindAllStringSubmatch(attrsText, -1) {
		attrs[match[1]] = html.UnescapeString(match[2])
	}
	return attrs
}

// codeBuddyMarshalArguments renders parameters as a JSON object without HTML
// escaping, so "<" and "&" survive as characters written by the model. The values
// are strings and nested string maps only, so encoding cannot fail.
func codeBuddyMarshalArguments(params map[string]any) string {
	var out strings.Builder
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(params)
	return strings.TrimSuffix(out.String(), "\n")
}

// codeBuddyNewToolCallID returns a synthetic call id in the reference format:
// "call_" followed by 24 random hex characters.
func codeBuddyNewToolCallID() string {
	var buf [12]byte
	_, _ = rand.Read(buf[:]) // crypto/rand.Read never reports a failure
	return "call_" + hex.EncodeToString(buf[:])
}

// codeBuddySkipXMLIgnoredSection skips CDATA, HTML comments and processing
// instructions. blocked reports an unterminated section: everything after it is
// still streaming and no tag may be recognised there.
func codeBuddySkipXMLIgnoredSection(text []rune, i int) (next int, advanced, blocked bool) {
	if i >= len(text) {
		return i, false, false
	}
	if codeBuddyHasRunePrefix(text, i, codeBuddyCDATAStart) {
		end := codeBuddyIndexRunes(text, i+len(codeBuddyCDATAStart), codeBuddyCDATAEnd)
		if end == -1 {
			return len(text), false, true
		}
		return end + len(codeBuddyCDATAEnd), true, false
	}
	if codeBuddyHasRunePrefix(text, i, codeBuddyCommentStart) {
		end := codeBuddyIndexRunes(text, i+len(codeBuddyCommentStart), codeBuddyCommentEnd)
		if end == -1 {
			return len(text), false, true
		}
		return end + len(codeBuddyCommentEnd), true, false
	}
	if codeBuddyHasRunePrefix(text, i, codeBuddyPIStart) {
		end := codeBuddyIndexRunes(text, i+len(codeBuddyPIStart), codeBuddyPIEnd)
		if end == -1 {
			return len(text), false, true
		}
		return end + len(codeBuddyPIEnd), true, false
	}
	return i, false, false
}

// codeBuddyBacktickRunEnd returns the index after the backtick run starting at i.
func codeBuddyBacktickRunEnd(text []rune, i int) int {
	for i < len(text) && text[i] == '`' {
		i++
	}
	return i
}

// codeBuddyMarkdownCodeSpanEnd returns the end of the inline code span starting at
// start. found is false for a fence marker (three or more backticks) or a span
// without a closing run yet.
func codeBuddyMarkdownCodeSpanEnd(text []rune, start int) (end int, found bool) {
	if start >= len(text) || text[start] != '`' {
		return start, false
	}
	tickCount := codeBuddyBacktickRunEnd(text, start) - start
	if tickCount >= 3 {
		return start, false
	}
	for end = start + tickCount; end < len(text); {
		if text[end] != '`' {
			end++
			continue
		}
		endTickCount := codeBuddyBacktickRunEnd(text, end) - end
		if endTickCount == tickCount {
			return end + endTickCount, true
		}
		end += endTickCount
	}
	return len(text), false
}

// codeBuddySkipLine returns the index after the end of the line containing i.
func codeBuddySkipLine(text []rune, i int) int {
	if lineEnd := codeBuddyIndexRunes(text, i, codeBuddyNewline); lineEnd != -1 {
		return lineEnd + 1
	}
	return len(text)
}

// codeBuddyFindFenceEnd returns the index after the closing line of the fenced code
// block that opens at text[open] with marker, or -1 while the fence stays open.
func codeBuddyFindFenceEnd(text []rune, open int, marker []rune) int {
	for i := codeBuddySkipLine(text, open); i < len(text); i = codeBuddySkipLine(text, i) {
		if codeBuddyHasRunePrefix(text, i, marker) {
			return codeBuddySkipLine(text, i)
		}
	}
	return -1
}

// codeBuddyScanIgnoredRegion reports whether a region the scanner ignores starts at
// i and, if so, where it ends. blocked reports a region that is still open at the
// end of text: nothing after it may be recognised yet, and the region must stay
// buffered with its opener so that its content is never taken for markup.
// startsLine tells whether text[0] sits at the start of a line, which is what makes
// a fence marker a fence.
func codeBuddyScanIgnoredRegion(text []rune, i int, startsLine bool) (next int, advanced, blocked bool) {
	if (i == 0 && startsLine) || (i > 0 && text[i-1] == '\n') {
		for _, marker := range codeBuddyFenceMarkers {
			matched := 0
			for matched < len(marker) && i+matched < len(text) && text[i+matched] == marker[matched] {
				matched++
			}
			if matched == 0 {
				continue
			}
			if matched < len(marker) {
				if i+matched == len(text) {
					// The marker is still arriving: hold it until it completes.
					return i, false, true
				}
				continue
			}
			if end := codeBuddyFindFenceEnd(text, i, marker); end != -1 {
				return end, true, false
			}
			return len(text), false, true
		}
	}
	if next, advanced, blocked := codeBuddySkipXMLIgnoredSection(text, i); advanced || blocked {
		return next, advanced, blocked
	}
	if text[i] == '`' {
		if end := codeBuddyBacktickRunEnd(text, i); end-i >= 3 {
			// Three or more backticks away from the start of a line are not an inline
			// code span: skip the whole run so that its tail is not read as one.
			return end, true, false
		}
		if end, found := codeBuddyMarkdownCodeSpanEnd(text, i); found {
			return end, true, false
		}
		return len(text), false, true
	}
	return i, false, false
}

// codeBuddyHoldFrom returns the index from which the buffer must stay buffered
// because it may still be part of markup, or len(buffer) when all of it may be
// emitted as text. Only markup outside a region the scanner ignores can be held:
// holding inside such a region would emit its opener and leave its closer behind.
func codeBuddyHoldFrom(buffer []rune, startsLine bool) int {
	lastLt := -1
	for i := 0; i < len(buffer); {
		next, advanced, blocked := codeBuddyScanIgnoredRegion(buffer, i, startsLine)
		if blocked {
			// The region is still open: keep its opener with it.
			return i
		}
		if advanced {
			i = next
			continue
		}
		if buffer[i] == '<' {
			lastLt = i
		}
		i++
	}
	if lastLt != -1 && codeBuddyIsToolCallStart(buffer[lastLt:]) {
		return lastLt
	}
	return len(buffer)
}

// codeBuddyFindToolMarkupTagOutsideIgnored returns the next tool markup tag at or
// after start, skipping CDATA, comments, processing instructions, inline code
// spans and fenced code blocks.
func codeBuddyFindToolMarkupTagOutsideIgnored(text []rune, start int, startsLine bool) (codeBuddyToolMarkupTag, bool) {
	i := start
	if i < 0 {
		i = 0
	}
	for i < len(text) {
		next, advanced, blocked := codeBuddyScanIgnoredRegion(text, i, startsLine)
		if blocked {
			return codeBuddyToolMarkupTag{}, false
		}
		if advanced {
			i = next
			continue
		}
		if tag, found := codeBuddyScanToolMarkupTagAt(text, i); found {
			return tag, true
		}
		i++
	}
	return codeBuddyToolMarkupTag{}, false
}

// codeBuddyScanToolMarkupTagAt scans one tag that starts at start, which must be
// its '<'.
func codeBuddyScanToolMarkupTagAt(text []rune, start int) (codeBuddyToolMarkupTag, bool) {
	if start >= len(text) || text[start] != '<' {
		return codeBuddyToolMarkupTag{}, false
	}
	idx := start + 1
	closing := false
	if idx < len(text) && text[idx] == '/' {
		closing = true
		idx++
	}
	for idx < len(text) && codeBuddyIsXMLSpace(text[idx]) {
		idx++
	}
	if idx >= len(text) {
		return codeBuddyToolMarkupTag{}, false
	}
	name, nameEnd, _ := codeBuddyMatchToolMarkupName(text, idx)
	if name == "" {
		return codeBuddyToolMarkupTag{}, false
	}
	if nameEnd < len(text) {
		switch {
		case codeBuddyIsXMLSpace(text[nameEnd]), text[nameEnd] == '>', text[nameEnd] == '/':
			// A plausible attribute, tag end or self closing marker.
		default:
			// The name runs into something that cannot follow a tag name, so this is
			// plain text such as "<50%".
			return codeBuddyToolMarkupTag{}, false
		}
	}
	attrStart := nameEnd
	for idx = nameEnd; idx < len(text); idx++ {
		switch {
		case text[idx] == '<':
			// A tag cannot span the start of another tag, so plain text such as
			// "a < b" is never swallowed up to a later ">".
			return codeBuddyToolMarkupTag{}, false
		case text[idx] == '>':
			return codeBuddyToolMarkupTag{
				start:      start,
				end:        idx,
				name:       name,
				closing:    closing,
				attributes: strings.TrimSpace(string(text[attrStart:idx])),
			}, true
		case text[idx] == '/' && idx+1 < len(text) && text[idx+1] == '>':
			return codeBuddyToolMarkupTag{
				start:      start,
				end:        idx + 1,
				name:       name,
				closing:    closing,
				attributes: strings.TrimSpace(string(text[attrStart:idx])),
			}, true
		}
	}
	return codeBuddyToolMarkupTag{}, false
}

// codeBuddyMatchToolMarkupName reads the tag name at start, optionally preceded by
// a DSML marker, and maps it to its canonical name. An unknown name is returned
// lowercased as is, because parameter elements may use any name.
func codeBuddyMatchToolMarkupName(text []rune, start int) (name string, end int, dsmlLike bool) {
	idx := start
	if afterMarker, ok := codeBuddyMatchDSMLPrefix(text, idx); ok {
		idx = afterMarker
		dsmlLike = true
	}
	nameStart := idx
	nameEnd := idx
	for nameEnd < len(text) && codeBuddyIsTagNameRune(text[nameEnd]) {
		nameEnd++
	}
	if nameEnd == nameStart {
		return "", start, false
	}
	rawName := strings.ToLower(string(text[nameStart:nameEnd]))
	for _, entry := range codeBuddyToolMarkupNames {
		if rawName != entry.raw {
			continue
		}
		if entry.dsmlOnly && !dsmlLike {
			continue
		}
		return entry.canonical, nameEnd, dsmlLike
	}
	return rawName, nameEnd, dsmlLike
}

// codeBuddyMatchDSMLPrefix consumes a DSML marker at idx, also accepting the
// marker written with fullwidth ASCII characters.
func codeBuddyMatchDSMLPrefix(text []rune, idx int) (end int, found bool) {
	for _, variant := range codeBuddyDSMLVariants {
		if codeBuddyHasRunePrefix(text, idx, variant) {
			return idx + len(variant), true
		}
	}
	if idx+8 <= len(text) {
		normalized := make([]rune, 0, 8)
		for pos := idx; pos < idx+8; pos++ {
			normalized = append(normalized, codeBuddyNormalizeFullwidth(text[pos]))
		}
		for _, variant := range codeBuddyDSMLVariants {
			if string(normalized) == string(variant) {
				return idx + 8, true
			}
		}
	}
	return idx, false
}

// codeBuddyNormalizeFullwidth maps a fullwidth ASCII character (U+FF01-U+FF5E)
// back to its ASCII equivalent.
func codeBuddyNormalizeFullwidth(r rune) rune {
	if r >= 0xFF01 && r <= 0xFF5E {
		return r - 0xFEE0
	}
	return r
}

// codeBuddyFindMatchingToolMarkupClose returns the close tag matching openTag,
// counting nested elements of the same name and skipping ignored regions.
func codeBuddyFindMatchingToolMarkupClose(text []rune, openTag codeBuddyToolMarkupTag) (codeBuddyToolMarkupTag, bool) {
	depth := 1
	for i := openTag.end + 1; i < len(text); {
		tag, found := codeBuddyFindToolMarkupTagOutsideIgnored(text, i, false)
		if !found {
			break
		}
		if tag.name == openTag.name {
			if tag.closing {
				depth--
				if depth == 0 {
					return tag, true
				}
			} else {
				depth++
			}
		}
		i = tag.end + 1
	}
	return codeBuddyToolMarkupTag{}, false
}

// codeBuddyIsToolCallStart reports whether a held-back tail starting with '<' may
// still become markup: the beginning of a tool call block, the opener of a region
// the scanner ignores, or a partial DSML marker. Plain text such as "<50%" or
// "a < b" is not held back.
func codeBuddyIsToolCallStart(tail []rune) bool {
	if len(tail) == 0 || tail[0] != '<' {
		return false
	}
	for _, r := range tail {
		if r == '>' {
			// The tag is already closed, so it is complete text rather than a tag
			// that is still being generated.
			return false
		}
	}
	body := tail[1:]
	if end, ok := codeBuddyMatchDSMLPrefix(body, 0); ok {
		body = body[end:]
	}
	body = []rune(strings.TrimLeftFunc(string(body), unicode.IsSpace))
	if len(body) == 0 {
		// "<" or "<｜｜DSML｜｜" with the tag name still to come.
		return true
	}
	if codeBuddyIsPartialDSMLMarker(body) || codeBuddyIsPartialOpener(body) {
		// A chunk boundary inside the DSML marker, or inside the opener of an
		// ignored region ("<![CDATA[", "<!--", "<?"): keep it buffered so the text
		// is still recognised once the rest of the opener arrives.
		return true
	}
	name := strings.ToLower(string(body))
	for _, opener := range codeBuddyToolCallOpeners {
		if strings.HasPrefix(opener, name) || strings.HasPrefix(name, opener) {
			return true
		}
	}
	return false
}

// codeBuddyIsPartialDSMLMarker reports whether body is a proper prefix of a DSML
// marker variant, so a chunk boundary inside the marker does not leak the marker
// into the emitted text.
func codeBuddyIsPartialDSMLMarker(body []rune) bool {
	normalized := make([]rune, len(body))
	for i, r := range body {
		normalized[i] = codeBuddyNormalizeFullwidth(r)
	}
	for _, variant := range codeBuddyDSMLVariants {
		if len(normalized) < len(variant) && codeBuddyHasRunePrefix(variant, 0, normalized) {
			return true
		}
	}
	return false
}

// codeBuddyIsPartialOpener reports whether a tag tail is a proper prefix of an
// ignored section opener, so a chunk boundary inside the opener neither leaks it
// into the emitted text nor loses the region it starts.
func codeBuddyIsPartialOpener(body []rune) bool {
	for _, opener := range codeBuddyIgnoredSectionOpeners {
		rest := opener[1:] // the tail excludes the leading '<'
		if len(body) < len(rest) && codeBuddyHasRunePrefix(rest, 0, body) {
			return true
		}
	}
	return false
}

// codeBuddyIsXMLSpace reports whether r is one of the whitespace characters the
// scanner accepts as tag padding.
func codeBuddyIsXMLSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\r' || r == '\n'
}

// codeBuddyIsTagNameRune reports whether r may appear in a tag name.
func codeBuddyIsTagNameRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-'
}

// codeBuddyHasRunePrefix reports whether text[start:] begins with prefix.
func codeBuddyHasRunePrefix(text []rune, start int, prefix []rune) bool {
	if start < 0 || start+len(prefix) > len(text) {
		return false
	}
	for i, r := range prefix {
		if text[start+i] != r {
			return false
		}
	}
	return true
}

// codeBuddyIndexRunes returns the first index at or after from where needle
// starts, or -1.
func codeBuddyIndexRunes(text []rune, from int, needle []rune) int {
	if from < 0 {
		from = 0
	}
	for i := from; i+len(needle) <= len(text); i++ {
		if codeBuddyHasRunePrefix(text, i, needle) {
			return i
		}
	}
	return -1
}
