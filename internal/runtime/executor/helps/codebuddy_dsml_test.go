package helps

import (
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// codeBuddyStreamParts feeds parts to a fresh parser and returns the concatenated
// text emitted while streaming, the flushed residual and every detected tool call.
func codeBuddyStreamParts(t *testing.T, parts []string) (string, string, []CodeBuddyDSMLToolCall) {
	t.Helper()
	parser := NewCodeBuddyDSMLParser()
	var emitted strings.Builder
	var calls []CodeBuddyDSMLToolCall
	for _, part := range parts {
		chunk, detected := parser.Feed(part)
		codeBuddyAssertEmittable(t, chunk)
		emitted.WriteString(chunk)
		calls = append(calls, detected...)
	}
	residual, detected := parser.Flush()
	if !utf8.ValidString(residual) {
		t.Fatalf("residual %q is not valid UTF-8", residual)
	}
	calls = append(calls, detected...)
	return emitted.String(), residual, calls
}

// codeBuddyAssertEmittable checks the invariants of text that a streaming parser
// hands to the client: it is valid UTF-8 and it never ends inside a DSML marker,
// which would leak a marker that the next chunk completes.
func codeBuddyAssertEmittable(t *testing.T, chunk string) {
	t.Helper()
	if !utf8.ValidString(chunk) {
		t.Fatalf("emitted text %q is not valid UTF-8", chunk)
	}
	for _, variant := range codeBuddyDSMLVariants {
		opener := append([]rune{'<'}, variant...)
		for n := 1; n < len(opener); n++ {
			if prefix := string(opener[:n]); strings.HasSuffix(chunk, prefix) {
				t.Fatalf("emitted text %q ends with the partial markup prefix %q", chunk, prefix)
			}
		}
	}
}

// codeBuddyStreamThrough feeds text in chunks of chunkSize runes.
func codeBuddyStreamThrough(t *testing.T, text string, chunkSize int) (string, string, []CodeBuddyDSMLToolCall) {
	t.Helper()
	runes := []rune(text)
	parts := make([]string, 0, len(runes)/chunkSize+1)
	for start := 0; start < len(runes); start += chunkSize {
		end := start + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		parts = append(parts, string(runes[start:end]))
	}
	return codeBuddyStreamParts(t, parts)
}

// codeBuddyCalls compares detected calls by name and arguments: ids are random.
func codeBuddyCalls(t *testing.T, calls []CodeBuddyDSMLToolCall, wantNames []string, wantArgs []map[string]any) {
	t.Helper()
	if len(calls) != len(wantNames) {
		t.Fatalf("got %d tool calls (%v), want %d", len(calls), codeBuddyCallNames(calls), len(wantNames))
	}
	for i, call := range calls {
		if call.Name != wantNames[i] {
			t.Errorf("call %d: name = %q, want %q", i, call.Name, wantNames[i])
		}
		if !strings.HasPrefix(call.ID, "call_") || len(call.ID) != len("call_")+24 {
			t.Errorf("call %d: id = %q, want call_ followed by 24 hex characters", i, call.ID)
		}
		if _, err := hex.DecodeString(call.ID[len("call_"):]); err != nil {
			t.Errorf("call %d: id = %q is not hexadecimal: %v", i, call.ID, err)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(call.Arguments), &got); err != nil {
			t.Fatalf("call %d: arguments %q are not a JSON object: %v", i, call.Arguments, err)
		}
		if !reflect.DeepEqual(got, wantArgs[i]) {
			t.Errorf("call %d: arguments = %v, want %v", i, got, wantArgs[i])
		}
	}
}

func codeBuddyCallNames(calls []CodeBuddyDSMLToolCall) []string {
	names := make([]string, 0, len(calls))
	for _, call := range calls {
		names = append(names, call.Name)
	}
	return names
}

func TestCodeBuddyDSMLParseCases(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		names      []string
		args       []map[string]any
		wantText   string
		assertText bool
		keepText   bool // the text must come back exactly as it went in
	}{
		{
			name:       "wrapped tool calls",
			input:      `<tool_calls><invoke name="bash"><parameter name="cmd">pwd</parameter></invoke></tool_calls>`,
			names:      []string{"bash"},
			args:       []map[string]any{{"cmd": "pwd"}},
			assertText: true,
		},
		{
			name: "fullwidth dsml marker",
			input: `<｜｜DSML｜｜tool_calls><｜｜DSML｜｜invoke name="bash">` +
				`<｜｜DSML｜｜parameter name="cmd">ls -la</｜｜DSML｜｜parameter>` +
				`</｜｜DSML｜｜invoke></｜｜DSML｜｜tool_calls>`,
			names:      []string{"bash"},
			args:       []map[string]any{{"cmd": "ls -la"}},
			assertText: true,
		},
		{
			name:       "halfwidth dsml marker",
			input:      `<||DSML||invoke name="bash"><||DSML||parameter name="command" string="true">ls</||DSML||parameter></||DSML||invoke>`,
			names:      []string{"bash"},
			args:       []map[string]any{{"command": "ls"}},
			assertText: true,
		},
		{
			name:       "short dsml marker",
			input:      `<|DSML|invoke name="bash"><|DSML|parameter name="cmd">ls</|DSML|parameter></|DSML|invoke>`,
			names:      []string{"bash"},
			args:       []map[string]any{{"cmd": "ls"}},
			assertText: true,
		},
		{
			name:       "simplified parameter element",
			input:      `<tool_calls><invoke name="bash"><cmd>ls -la</cmd></invoke></tool_calls>`,
			names:      []string{"bash"},
			args:       []map[string]any{{"cmd": "ls -la"}},
			assertText: true,
		},
		{
			name:       "unknown wrapper name is kept as text",
			input:      `<tool_call><invoke name="exec_command"><cmd>pwd && ls -la</cmd></invoke></tool_call>`,
			names:      []string{"exec_command"},
			args:       []map[string]any{{"cmd": "pwd && ls -la"}},
			wantText:   `<tool_call></tool_call>`,
			assertText: true,
		},
		{
			name:       "bare invoke without wrapper",
			input:      `<invoke name="exec_command"><cmd>pwd</cmd></invoke>`,
			names:      []string{"exec_command"},
			args:       []map[string]any{{"cmd": "pwd"}},
			assertText: true,
		},
		{
			name:       "multiple simple parameters",
			input:      `<invoke name="bash"><command>ls</command><cwd>/tmp</cwd></invoke>`,
			names:      []string{"bash"},
			args:       []map[string]any{{"command": "ls", "cwd": "/tmp"}},
			assertText: true,
		},
		{
			name:       "dsml parameter wins over simple element",
			input:      `<invoke name="test"><||DSML||parameter name="key" string="true">dsml_value</||DSML||parameter><key>simple_value</key></invoke>`,
			names:      []string{"test"},
			args:       []map[string]any{{"key": "dsml_value"}},
			assertText: true,
		},
		{
			name:       "empty and populated parameters",
			input:      `<invoke name="test"><param1></param1><param2>value</param2></invoke>`,
			names:      []string{"test"},
			args:       []map[string]any{{"param1": "", "param2": "value"}},
			assertText: true,
		},
		{
			name:       "html entities are decoded",
			input:      `<invoke name="bash"><command>echo &lt;test&gt;</command></invoke>`,
			names:      []string{"bash"},
			args:       []map[string]any{{"command": "echo <test>"}},
			assertText: true,
		},
		{
			name:       "nested parameter groups",
			input:      `<invoke name="outer"><inner><a>1</a><b>2</b></inner></invoke>`,
			names:      []string{"outer"},
			args:       []map[string]any{{"inner": map[string]any{"a": "1", "b": "2"}}},
			assertText: true,
		},
		{
			name:       "cdata in dsml parameter",
			input:      "<||DSML||invoke name=\"bash\">\n<||DSML||parameter name=\"command\"><![CDATA[pwd && ls -la]]></||DSML||parameter>\n</||DSML||invoke>",
			names:      []string{"bash"},
			args:       []map[string]any{{"command": "pwd && ls -la"}},
			assertText: true,
		},
		{
			name:       "cdata keeps special characters",
			input:      "<invoke name=\"bash\">\n<command><![CDATA[echo \"<hello>\" && grep \"&\" | wc -l]]></command>\n</invoke>",
			names:      []string{"bash"},
			args:       []map[string]any{{"command": `echo "<hello>" && grep "&" | wc -l`}},
			assertText: true,
		},
		{
			name: "cdata keeps xml markup",
			input: "<invoke name=\"write_file\">\n<path>config.xml</path>\n" +
				"<content><![CDATA[\n<configuration>\n    <database>\n        <host>localhost</host>\n    </database>\n</configuration>\n]]></content>\n</invoke>",
			names: []string{"write_file"},
			args: []map[string]any{{
				"path": "config.xml",
				"content": "\n<configuration>\n    <database>\n        <host>localhost</host>\n" +
					"    </database>\n</configuration>\n",
			}},
			assertText: true,
		},
		{
			name: "cdata keeps a heredoc",
			input: "<invoke name=\"bash\">\n<command><![CDATA[\n" +
				"git commit -m \"$(cat <<'EOF'\nfeat: add new feature\n\nThis commit adds:\n- Feature A\n- Feature B\nEOF\n)\"\n" +
				"]]></command>\n</invoke>",
			names: []string{"bash"},
			args: []map[string]any{{
				"command": "\ngit commit -m \"$(cat <<'EOF'\nfeat: add new feature\n\nThis commit adds:\n- Feature A\n- Feature B\nEOF\n)\"\n",
			}},
			assertText: true,
		},
		{
			name:       "cdata is not html decoded",
			input:      `<invoke name="test"><param1><![CDATA[&lt;raw&gt;]]></param1><param2>&lt;decoded&gt;</param2></invoke>`,
			names:      []string{"test"},
			args:       []map[string]any{{"param1": "&lt;raw&gt;", "param2": "<decoded>"}},
			assertText: true,
		},
		{
			name:       "empty cdata",
			input:      `<invoke name="test"><param><![CDATA[]]></param></invoke>`,
			names:      []string{"test"},
			args:       []map[string]any{{"param": ""}},
			assertText: true,
		},
		{
			name:     "ignore fenced code block",
			input:    "这是一个示例：\n\n```xml\n<invoke name=\"bash\">\n<command>rm -rf /</command>\n</invoke>\n```\n\n这不应该被解析。",
			names:    []string{},
			args:     []map[string]any{},
			keepText: true,
		},
		{
			name:     "ignore tilde code block",
			input:    "示例代码：\n\n~~~python\n<invoke name=\"eval\">\n<code>import os</code>\n</invoke>\n~~~\n\n不应该执行。",
			names:    []string{},
			args:     []map[string]any{},
			keepText: true,
		},
		{
			name:       "ignore inline code span",
			input:      "示例：`<invoke name=\"bash\"><command>pwd</command></invoke>`\n\n实际执行：<invoke name=\"bash\"><command>ls</command></invoke>",
			names:      []string{"bash"},
			args:       []map[string]any{{"command": "ls"}},
			assertText: false,
		},
		{
			name:       "ignore xml comment",
			input:      "执行：\n<invoke name=\"bash\"><command>ls</command></invoke>\n\n<!-- 注释中的示例：\n<invoke name=\"bash\"><command>rm -rf /</command></invoke>\n-->\n\n完成。",
			names:      []string{"bash"},
			args:       []map[string]any{{"command": "ls"}},
			assertText: false,
		},
		{
			name:       "ignore comment inside code block",
			input:      "示例：\n\n```xml\n<!-- 这是注释 -->\n<invoke name=\"bash\"><command>example</command></invoke>\n```\n\n实际：<invoke name=\"bash\"><command>real</command></invoke>",
			names:      []string{"bash"},
			args:       []map[string]any{{"command": "real"}},
			assertText: false,
		},
		{
			name:       "ignore cdata inside comment",
			input:      "执行：\n<invoke name=\"bash\">\n<command><![CDATA[pwd]]></command>\n</invoke>\n\n<!-- 示例：\n<invoke name=\"bash\">\n<command><![CDATA[rm -rf /]]></command>\n</invoke>\n-->",
			names:      []string{"bash"},
			args:       []map[string]any{{"command": "pwd"}},
			assertText: false,
		},
		{
			name:       "ignore processing instruction",
			input:      "<?xml version=\"1.0\"?><invoke name=\"bash\"><cmd>ls</cmd></invoke>",
			names:      []string{"bash"},
			args:       []map[string]any{{"cmd": "ls"}},
			assertText: false,
		},
		{
			name:     "invoke without name is ignored",
			input:    `<invoke><cmd>ls</cmd></invoke>`,
			names:    []string{},
			args:     []map[string]any{},
			keepText: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, calls := CodeBuddyParseDSML(tc.input)
			codeBuddyCalls(t, calls, tc.names, tc.args)
			if tc.keepText && text != tc.input {
				t.Errorf("text = %q, want the input unchanged", text)
			}
			if tc.assertText && text != tc.wantText {
				t.Errorf("text = %q, want %q", text, tc.wantText)
			}
		})
	}
}

func TestCodeBuddyDSMLParseMultipleCallsAndNestedFence(t *testing.T) {
	// Ports the reference test for nested code blocks: the simple fence scanner
	// cannot handle nesting, so only require that the code outside a fence is
	// still parsed and that the "safe" call survives.
	input := "外层代码块：\n\n```markdown\n# 文档示例\n\n```xml\n" +
		"<invoke name=\"bash\"><command>dangerous</command></invoke>\n```\n\n这是嵌套的代码块。\n```\n\n" +
		"实际执行：<invoke name=\"bash\"><command>safe</command></invoke>"

	_, calls := CodeBuddyParseDSML(input)
	if len(calls) == 0 {
		t.Fatalf("expected at least one tool call")
	}
	foundSafe := false
	for _, call := range calls {
		if strings.Contains(call.Arguments, `"safe"`) {
			foundSafe = true
		}
	}
	if !foundSafe {
		t.Errorf("the call outside the fence is missing, got %v", calls)
	}
}

func TestCodeBuddyDSMLParseOutsideFence(t *testing.T) {
	input := "执行命令：\n\n<invoke name=\"bash\"><command>ls -la</command></invoke>\n\n" +
		"代码示例：\n```\n<invoke name=\"bash\"><command>rm -rf /</command></invoke>\n```\n\n" +
		"再执行一个：\n<invoke name=\"bash\"><command>pwd</command></invoke>\n"

	text, calls := CodeBuddyParseDSML(input)
	codeBuddyCalls(t, calls,
		[]string{"bash", "bash"},
		[]map[string]any{{"command": "ls -la"}, {"command": "pwd"}})
	if !strings.Contains(text, "rm -rf /") {
		t.Errorf("fenced code should stay in the text, got %q", text)
	}
	if strings.Contains(text, "ls -la") || strings.Contains(text, "pwd") {
		t.Errorf("parsed calls must not leak into the text, got %q", text)
	}
}

func TestCodeBuddyDSMLStreamingChunked(t *testing.T) {
	// Ports test_dsml_residual_buffer.py and test_c3_inline_tool_calls.py: text that
	// only looks like markup must never be held back or dropped.
	docsWithExactText := map[string]string{
		"tag examples in prose": "前文内容……整体架构分层如下。支持三种格式：DeepSeek DSML（" +
			"<｜｜DSML｜｜>）、Claude 风格（<tool_call><invoke>）示例说明。" +
			"第三节 脱敏模块：desensitize.py 对敏感词插入零宽空格以缓解审核误拦。" +
			"第四节 上下文压缩：responses_projection.py 做最小语义闭包省 token。" +
			"第五节 日志系统：proxy 记录每次请求的 metadata 与耗时。" +
			"需要我针对某个具体模块深入分析，或指出代码中存在的问题清单吗？",
		"literal less than signs": "当前进度 <50%，且 a < b 时触发分支，流程结束。",
		"inline code tag":         "前文：自动修复缺失 `<tool_calls>` 包装器，把文本标记转成标准 `tool_calls`。",
		"inline code fragment":    "使用 `<tool_calls>` 标记包裹工具调用。",
		"unclosed inline code":    "写法：`<tool_calls> 尚未闭合",
		"truncated tool calls":    "开始处理<tool_calls><invoke name=\"bash\"><parameter name=\"cmd\">",
		"truncated bare invoke":   "开始<tool_calls><invoke name=\"bash\"><parameter name=\"cmd\">",
	}
	for name, text := range docsWithExactText {
		for _, chunkSize := range []int{1, 2, 3, 8, 10, 12, 16, 20, 64} {
			emitted, residual, calls := codeBuddyStreamThrough(t, text, chunkSize)
			if len(calls) != 0 {
				t.Errorf("%s (chunk %d): unexpected tool calls %v", name, chunkSize, codeBuddyCallNames(calls))
			}
			if got := emitted + residual; got != text {
				t.Errorf("%s (chunk %d): text = %q, want %q", name, chunkSize, got, text)
			}
		}
	}
}

func TestCodeBuddyDSMLStreamingDetectsRealCalls(t *testing.T) {
	// Ports test_real_tool_call_still_detected from both regression modules.
	input := "先看下目录结构：<tool_calls>" +
		"<invoke name=\"bash\"><parameter name=\"cmd\">ls -la</parameter></invoke>" +
		"</tool_calls>已列出。"
	for _, chunkSize := range []int{1, 3, 8, 12, 32, 128} {
		emitted, residual, calls := codeBuddyStreamThrough(t, input, chunkSize)
		codeBuddyCalls(t, calls, []string{"bash"}, []map[string]any{{"cmd": "ls -la"}})
		text := emitted + residual
		if strings.Contains(text, "ls -la") {
			t.Errorf("chunk %d: call arguments leaked into the text: %q", chunkSize, text)
		}
		if !strings.Contains(text, "先看下目录结构：") || !strings.Contains(text, "已列出。") {
			t.Errorf("chunk %d: surrounding text was lost: %q", chunkSize, text)
		}
	}
}

func TestCodeBuddyDSMLStreamingMarkerSplit(t *testing.T) {
	// A chunk boundary inside the DSML marker must not leak the marker as text:
	// the call is only recognisable once the marker has arrived completely.
	input := "<｜｜DSML｜｜invoke name=\"bash\"><｜｜DSML｜｜parameter name=\"cmd\">ls</｜｜DSML｜｜parameter></｜｜DSML｜｜invoke>"
	for split := 1; split < len([]rune(input)); split++ {
		runes := []rune(input)
		emitted, residual, calls := codeBuddyStreamParts(t, []string{string(runes[:split]), string(runes[split:])})
		if len(calls) != 1 || calls[0].Name != "bash" {
			t.Fatalf("split %d: calls = %v, want one bash call", split, codeBuddyCallNames(calls))
		}
		if text := emitted + residual; text != "" {
			t.Errorf("split %d: markup leaked into the text: %q", split, text)
		}
	}
}

func TestCodeBuddyDSMLStreamingMatchesOneShot(t *testing.T) {
	docs := map[string]string{
		"plain text": "当前进度 <50%，且 a < b 时触发分支，流程结束。",
		"wrapped and bare": "先看下目录结构：<tool_calls><invoke name=\"bash\">" +
			"<parameter name=\"cmd\">ls -la</parameter></invoke></tool_calls>已列出。\n" +
			"然后 <invoke name=\"read\"><path>/tmp/a.txt</path></invoke> 结束。",
		"dsml markers": "<｜｜DSML｜｜tool_calls><｜｜DSML｜｜invoke name=\"bash\">" +
			"<｜｜DSML｜｜parameter name=\"cmd\">pwd</｜｜DSML｜｜parameter>" +
			"</｜｜DSML｜｜invoke></｜｜DSML｜｜tool_calls>后面还有文本",
		"inline code and fence": "示例：`<tool_calls>` 与\n```xml\n" +
			"<invoke name=\"bash\"><command>rm -rf /</command></invoke>\n```\n" +
			"实际：<||DSML||invoke name=\"echo\"><text>hi</text></||DSML||invoke>",
		"cdata and nested": "<invoke name=\"write\"><content><![CDATA[<a>\n<b>]]></content>" +
			"<opts><verbose>1</verbose></opts></invoke>",
		"truncated markup": "开始<tool_calls><invoke name=\"bash\"><parameter name=\"cmd\">",
		"prose before and after": "the model says: use <tool_calls> with <invoke name=\"x\">, " +
			"then it really calls: <invoke name=\"real\"><arg>1</arg></invoke> done.",
	}

	// wantNames pins the calls the one-shot parse must return for each document:
	// without it the equivalence check below would also pass for a parser that
	// silently finds no call at all.
	wantNames := map[string][]string{
		"plain text":             {},
		"wrapped and bare":       {"bash", "read"},
		"dsml markers":           {"bash"},
		"inline code and fence":  {"echo"},
		"cdata and nested":       {"write"},
		"truncated markup":       {},
		"prose before and after": {"real"},
	}

	for name, doc := range docs {
		names, ok := wantNames[name]
		if !ok {
			t.Fatalf("no expected calls recorded for %q", name)
		}
		wantText, wantCalls := CodeBuddyParseDSML(doc)
		if len(wantCalls) != len(names) {
			t.Fatalf("one-shot calls of %q = %v, want %v", name, codeBuddyCallNames(wantCalls), names)
		}
		for i, wantName := range names {
			if wantCalls[i].Name != wantName {
				t.Errorf("one-shot call %d of %q = %q, want %q", i, name, wantCalls[i].Name, wantName)
			}
		}
		runes := []rune(doc)
		t.Run(name, func(t *testing.T) {
			check := func(label string, parts []string) {
				t.Helper()
				emitted, residual, calls := codeBuddyStreamParts(t, parts)
				if got := emitted + residual; got != wantText {
					t.Errorf("%s: text = %q, want the one-shot text %q", label, got, wantText)
				}
				if len(calls) != len(wantCalls) {
					t.Fatalf("%s: got %d calls (%v), want %d",
						label, len(calls), codeBuddyCallNames(calls), len(wantCalls))
				}
				for i := range calls {
					if calls[i].Name != wantCalls[i].Name || calls[i].Arguments != wantCalls[i].Arguments {
						t.Errorf("%s: call %d = %s %s, want %s %s",
							label, i, calls[i].Name, calls[i].Arguments, wantCalls[i].Name, wantCalls[i].Arguments)
					}
				}
			}

			for _, chunkSize := range []int{1, 2, 3, 4, 5, 8, 10, 12, 16, 20, 64} {
				parts := make([]string, 0, len(runes)/chunkSize+1)
				for start := 0; start < len(runes); start += chunkSize {
					end := start + chunkSize
					if end > len(runes) {
						end = len(runes)
					}
					parts = append(parts, string(runes[start:end]))
				}
				check("chunk size "+string(rune('0'+chunkSize%10)), parts)
			}
			// Split at every single rune boundary into exactly two parts.
			for split := 1; split < len(runes); split++ {
				check("split at "+string(runes[split]), []string{string(runes[:split]), string(runes[split:])})
			}
		})
	}
}

func TestCodeBuddyDSMLEmittedAndIDFormat(t *testing.T) {
	parser := NewCodeBuddyDSMLParser()
	if parser.Emitted() {
		t.Fatal("a fresh parser must not report emitted calls")
	}
	text, calls := parser.Feed("没有工具调用")
	if text != "没有工具调用" || len(calls) != 0 {
		t.Fatalf("Feed = (%q, %v), want the plain text", text, codeBuddyCallNames(calls))
	}
	if parser.Emitted() {
		t.Fatal("Emitted must stay false without a tool call")
	}
	if _, calls = parser.Feed(`<invoke name="bash"><cmd>ls</cmd></invoke>`); len(calls) != 1 {
		t.Fatalf("calls = %v, want one call", codeBuddyCallNames(calls))
	}
	if !parser.Emitted() {
		t.Fatal("Emitted must be true after a tool call")
	}
	if leftover, _ := parser.Flush(); leftover != "" {
		t.Fatalf("residual = %q, want empty", leftover)
	}
}

func TestCodeBuddyDSMLToolCallIDsAreUnique(t *testing.T) {
	input := `<tool_calls>` +
		`<invoke name="a"><cmd>1</cmd></invoke>` +
		`<invoke name="b"><cmd>2</cmd></invoke>` +
		`<invoke name="c"><cmd>3</cmd></invoke>` +
		`</tool_calls>`
	_, calls := CodeBuddyParseDSML(input)
	if len(calls) != 3 {
		t.Fatalf("calls = %v, want three calls", codeBuddyCallNames(calls))
	}
	seen := map[string]bool{}
	for _, call := range calls {
		if seen[call.ID] {
			t.Errorf("duplicate id %q", call.ID)
		}
		seen[call.ID] = true
	}
}

func TestCodeBuddyDSMLFlushConsumesBufferedCalls(t *testing.T) {
	// A chunk carrying two complete blocks: Feed returns the first one, so Flush
	// must consume the second instead of leaking its markup as text.
	input := "<invoke name=\"first\"><cmd>1</cmd></invoke>" +
		"<invoke name=\"second\"><cmd>2</cmd></invoke>"

	parser := NewCodeBuddyDSMLParser()
	text, calls := parser.Feed(input)
	codeBuddyCalls(t, calls, []string{"first"}, []map[string]any{{"cmd": "1"}})
	if text != "" {
		t.Errorf("Feed text = %q, want empty", text)
	}

	residual, calls := parser.Flush()
	codeBuddyCalls(t, calls, []string{"second"}, []map[string]any{{"cmd": "2"}})
	if residual != "" {
		t.Errorf("residual = %q, want empty", residual)
	}
}

func TestCodeBuddyDSMLStreamingSplitIndependence(t *testing.T) {
	// For every document, every two-part split at a rune boundary and every chunk
	// size must produce exactly what the one-shot parse produces. A chunk boundary
	// must never lose the context of a region the scanner ignores: markup inside a
	// region that leaked as text becomes a bogus tool call, and an agent client would
	// execute a real action for it.
	docs := []string{
		"",
		"hello world, a < b and <50% done",
		`x<tool_calls><invoke name="a"><cmd>1</cmd></invoke></tool_calls>y`,
		"<｜｜DSML｜｜tool_calls><｜｜DSML｜｜invoke name=\"a\"><｜｜DSML｜｜parameter name=\"cmd\">ls -la</｜｜DSML｜｜parameter></｜｜DSML｜｜invoke></｜｜DSML｜｜tool_calls>done",
		"<||DSML||invoke name=\"a\"><||DSML||parameter name=\"c\" string=\"true\">1</||DSML||parameter></||DSML||invoke>",
		"<|DSML|invoke name=\"a\"><|DSML|parameter name=\"c\">1</|DSML|parameter></|DSML|invoke>",
		"see:\n```xml\n<invoke name=\"a\"><cmd>1</cmd></invoke>\n```\ndone",
		"see:\n~~~\n<invoke name=\"a\"><cmd>1</cmd></invoke>\n~~~\ndone",
		"use `<invoke name=\"a\"><cmd>1</cmd></invoke>` ok",
		"text `code and `<tool_calls>` more",
		"x<![CDATA[<invoke name=\"a\"><cmd>1</cmd></invoke>]]>y",
		"x<!--<invoke name=\"a\"><cmd>1</cmd></invoke>-->y",
		"x<!--c<invoke name=\"a\"><cmd>1</cmd></invoke>-->y",
		"x<!--<tool_calls><invoke name=\"a\"><cmd>1</cmd></invoke></tool_calls>",
		"x<?pi<invoke name=\"a\"><cmd>1</cmd></invoke>?>y",
		"<invoke name=\"w\"><c><![CDATA[<invoke name=\"a\"><cmd>1</cmd></invoke>]]></c></invoke>",
		"<invoke name=\"a\"><p><![CDATA[x]]></p><g><b>2</b></g></invoke>",
		"start<tool_calls><invoke name=\"a\"><cmd>1",
		"<invoke name=\"a\"><cmd/></invoke>",
		"<invoke name=\"a\"><cmd>1</cmd></invoke><invoke name=\"b\"><cmd>2</cmd></invoke>",
		"前文<invoke name=\"a\"><c>1</c></invoke>中段<invoke name=\"b\"><c>2</c></invoke>后记",
		"<tool_call><invoke name=\"a\"><cmd>1</cmd></invoke></tool_call>",
		"use <tool_calls> with <invoke name=\"x\">, really <invoke name=\"real\"><arg>1</arg></invoke>.",
		"中文前言 说明：<｜｜DSML｜｜invoke name=\"a\"><｜｜DSML｜｜parameter name=\"c\">值</｜｜DSML｜｜parameter></｜｜DSML｜｜invoke>结束",
		"<invoke name=\"a\"><c>&lt;x&gt;&amp;</c></invoke>",
		"<invoke name=\"a\"><g><a>1</a><b>2</b></g></invoke>",
		"<invoke name=\"a\"><parameter name=\"k\">one</parameter><parameter name=\"k\">two</parameter></invoke>",
		"a\n```\nb\n```\nc\n~~~\nd\n~~~\ne",
		"  ```\ncode\n  ```\n",
		"```\ncode\n```\n<invoke name=\"a\"><cmd>1</cmd></invoke>",
		"see:\n```\n<!-- c -->\n<invoke name=\"a\"><cmd>1</cmd></invoke>\n```\ndone",
		"text <\ntext <!\ntext <![CD\ntext <!--\ntext <?",
		"prose text  \n  <invoke name=\"a\"><cmd>1</cmd></invoke>  \ntrailing  ",
		"mixed <人> 中 <｜｜DSML｜｜invoke name=\"a\"><｜｜DSML｜｜parameter name=\"c\"><![CDATA[值]]></｜｜DSML｜｜parameter></｜｜DSML｜｜invoke> 后\n" +
			"<!-- 示例 <invoke name=\"b\"><cmd>2</cmd></invoke> -->\n```\n<invoke name=\"d\"><cmd>3</cmd></invoke>\n```",
		// Reproducers for the scanner defects: a fence closed by the other marker, a
		// hold position inside an already closed region, backticks and comment openers
		// inside code, a code span inside a fence, a fence line inside CDATA, a mid-line
		// fence marker and a tag name without a boundary.
		"```markdown\n~~~\n<invoke name=\"a\"><cmd>rm -rf /</cmd></invoke>\n~~~\n```",
		"prose `<invoke` more\n<invoke name=\"a\"><cmd>1</cmd></invoke>",
		"prose `<!--` <invoke name=\"a\"><cmd>1</cmd></invoke>",
		"<!-- ` -->\n<invoke name=\"a\"><cmd>1</cmd></invoke>",
		"```\necho `date\n```\n<invoke name=\"a\"><cmd>1</cmd></invoke>",
		"```\n<invoke\n```\n<invoke name=\"a\"><cmd>1</cmd></invoke>",
		"<invoke name=\"w\"><content><![CDATA[\n```python\nprint(1)\n]]></content></invoke>",
		"```\nhi\n```\n``code`` <invoke name=\"a\"><cmd>1</cmd></invoke>",
		"use ```code``` here <invoke name=\"a\"><cmd>1</cmd></invoke>",
		"hello ~~~ world <invoke name=\"a\"><cmd>1</cmd></invoke>",
		"当前进度 <50%，且 a < b 时触发分支：<invoke name=\"bash\"><cmd>ls</cmd></invoke>",
		"the model says: use <tool_calls> with <invoke name=\"x\">, then it really calls: " +
			"<invoke name=\"real\"><arg>1</arg></invoke> done.",
	}

	// expectedCalls lists the tool calls every chunking of each document must produce,
	// in order: the equivalence check below would otherwise also pass for a parser that
	// finds no call at all, or for a streaming path that invents one.
	expectedCalls := [][]string{
		{}, {}, {"a"}, {"a"}, {"a"}, {"a"}, {}, {}, {}, {},
		{}, {}, {}, {}, {}, {"w"}, {"a"}, {}, {"a"}, {"a", "b"},
		{"a", "b"}, {"a"}, {"real"}, {"a"}, {"a"}, {"a"}, {"a"}, {}, {}, {"a"},
		{}, {}, {"a"}, {"a"}, {}, {"a"}, {"a"}, {"a"}, {"a"}, {"a"},
		{"w"}, {"a"}, {"a"}, {"a"}, {"bash"}, {"real"},
	}
	if len(expectedCalls) != len(docs) {
		t.Fatalf("expectedCalls has %d entries for %d documents", len(expectedCalls), len(docs))
	}

	for index, doc := range docs {
		wantText, wantCalls := CodeBuddyParseDSML(doc)
		if len(wantCalls) != len(expectedCalls[index]) {
			t.Fatalf("one-shot calls of %q = %v, want %v",
				doc, codeBuddyCallNames(wantCalls), expectedCalls[index])
		}
		for i, wantName := range expectedCalls[index] {
			if wantCalls[i].Name != wantName {
				t.Errorf("one-shot call %d of %q = %q, want %q", i, doc, wantCalls[i].Name, wantName)
			}
		}
		runes := []rune(doc)
		check := func(label string, parts []string) {
			t.Helper()
			emitted, residual, calls := codeBuddyStreamParts(t, parts)
			if got := emitted + residual; got != wantText {
				t.Errorf("doc %q %s: text = %q, want the one-shot text %q", doc, label, got, wantText)
				return
			}
			if len(calls) != len(wantCalls) {
				t.Fatalf("doc %q %s: got %d calls (%v), want %d",
					doc, label, len(calls), codeBuddyCallNames(calls), len(wantCalls))
			}
			for i := range calls {
				if calls[i].Name != wantCalls[i].Name || calls[i].Arguments != wantCalls[i].Arguments {
					t.Errorf("doc %q %s: call %d = %s %s, want %s %s",
						doc, label, i, calls[i].Name, calls[i].Arguments, wantCalls[i].Name, wantCalls[i].Arguments)
				}
			}
		}

		for split := 1; split < len(runes); split++ {
			check("split "+string(runes[split]), []string{string(runes[:split]), string(runes[split:])})
		}
		for _, chunkSize := range []int{1, 2, 3, 4, 5, 6, 8, 16} {
			parts := make([]string, 0, len(runes)/chunkSize+1)
			for start := 0; start < len(runes); start += chunkSize {
				end := start + chunkSize
				if end > len(runes) {
					end = len(runes)
				}
				parts = append(parts, string(runes[start:end]))
			}
			check("chunk size "+string(rune('0'+chunkSize%10)), parts)
		}
	}
}

func TestCodeBuddyDSMLParseKeepsTextAroundIncompleteMarkup(t *testing.T) {
	// The scanner could hold a position inside an already closed region and then swallow
	// the rest of that region, or let an unclosed tag stop a later complete block from
	// ever being recognised. Both lose or hide real content, so pin the exact text and
	// calls here instead of only comparing the streaming parse with the one-shot parse.
	cases := []struct {
		name  string
		input string
		text  string
		calls []string
	}{
		{
			name:  "partial markup inside a closed code span",
			input: "prose `<invoke` more\n<invoke name=\"a\"><cmd>1</cmd></invoke>",
			text:  "prose `<invoke` more\n",
			calls: []string{"a"},
		},
		{
			name: "unclosed wrapper in prose does not hide the later call",
			input: "the model says: use <tool_calls> with <invoke name=\"x\">, then it really calls: " +
				"<invoke name=\"real\"><arg>1</arg></invoke> done.",
			text:  "the model says: use <tool_calls> with <invoke name=\"x\">, then it really calls:  done.",
			calls: []string{"real"},
		},
		{
			name:  "tilde line inside a backtick fence stays code",
			input: "```markdown\n~~~\n<invoke name=\"a\"><cmd>rm -rf /</cmd></invoke>\n~~~\n```",
			text:  "```markdown\n~~~\n<invoke name=\"a\"><cmd>rm -rf /</cmd></invoke>\n~~~\n```",
			calls: []string{},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			text, calls := CodeBuddyParseDSML(testCase.input)
			if text != testCase.text {
				t.Errorf("text = %q, want %q", text, testCase.text)
			}
			if len(calls) != len(testCase.calls) {
				t.Fatalf("got %d tool calls (%v), want %v", len(calls), codeBuddyCallNames(calls), testCase.calls)
			}
			for i, wantName := range testCase.calls {
				if calls[i].Name != wantName {
					t.Errorf("call %d = %q, want %q", i, calls[i].Name, wantName)
				}
			}
		})
	}
}
