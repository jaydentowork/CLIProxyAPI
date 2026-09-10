package executor

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestClaudeOAuthUnresolvedNamespaceAlias(t *testing.T) {
	const original = "multi_agent_v1__wait_for_agents"
	for _, declared := range []bool{true, false} {
		t.Run(fmt.Sprintf("declared=%t", declared), func(t *testing.T) {
			tools := `{"name":"read_file","input_schema":{"type":"object"}}`
			if declared {
				tools += `,{"name":"multi_agent_v1__wait_for_agents","input_schema":{"type":"object"}}`
			}
			request := []byte(`{"tools":[` + tools + `],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_old","name":"multi_agent_v1__wait_for_agents","input":{}}]}]}`)
			upstream, reverseMap := prepareClaudeOAuthToolNamesForUpstream(request, claudeMCPAliasOptions{secret: "namespace-alias-caller"})
			server := claudeMCPAliasServer(gjson.GetBytes(upstream, "tools.0.name").String())
			name := "mcp__" + server + "__sentence_" + original
			want := name
			if declared {
				want = original
			}
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
					block := fmt.Sprintf(`{"type":"tool_use","id":"toolu_1","name":%q,"input":{"agent_ids":["agent_1"]}}`, name)
					payload := []byte(`{"content":[` + block + `]}`)
					path := "content.0"
					restore := restoreClaudeOAuthToolNamesFromResponse
					if stream {
						payload = []byte(`data: {"type":"content_block_start","index":0,"content_block":` + block + `}`)
						path = "content_block"
						restore = restoreClaudeOAuthToolNamesFromStreamLine
					}
					restored, err := restore(payload, reverseMap)
					if err != nil {
						t.Fatalf("restore tool name: %v", err)
					}
					if !declared && !bytes.Equal(restored, payload) {
						t.Fatalf("unresolved tool call changed: %s", restored)
					}
					json := bytes.TrimPrefix(restored, []byte("data: "))
					if got := gjson.GetBytes(json, path+".name").String(); got != want {
						t.Fatalf("tool name = %q, want %q", got, want)
					}
					if got := gjson.GetBytes(json, path+".input.agent_ids.0").String(); got != "agent_1" {
						t.Fatalf("tool arguments changed: %s", restored)
					}
				})
			}
		})
	}
}

func TestClaudeOAuthReportedUnresolvedAlias(t *testing.T) {
	const name = "mcp__kitten_slab__sentence_multi_agent_v1__wait_for_agents"
	reverseMap := map[string]string{"mcp__kitten_slab__sentence_read_file": "read_file"}
	line := []byte(fmt.Sprintf(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}}`, name))
	restored, err := restoreClaudeOAuthToolNamesFromStreamLine(line, reverseMap)
	if err != nil {
		t.Fatalf("well-formed unresolved tool terminated stream: %v", err)
	}
	if !bytes.Equal(restored, line) {
		t.Fatalf("unresolved tool call changed: %s", restored)
	}
}

func TestClaudeOAuthHybridMCPAliases(t *testing.T) {
	tests := []struct {
		name      string
		tools     []string
		suffix    string
		want      string
		wantError bool
	}{
		{name: "full MCP name", tools: []string{"mcp__inventory__lookup_by_id"}, suffix: "mcp__inventory__lookup_by_id", want: "mcp__inventory__lookup_by_id"},
		{name: "MCP prefix omitted", tools: []string{"mcp__inventory__lookup_by_id"}, suffix: "inventory__lookup_by_id", want: "mcp__inventory__lookup_by_id"},
		{name: "tool component only", tools: []string{"mcp__inventory__lookup_by_id"}, suffix: "lookup_by_id", want: "mcp__inventory__lookup_by_id"},
		{name: "ordinary alias takes precedence", tools: []string{"mcp__inventory__read_file"}, suffix: "abandon_read_file", want: "read_file"},
		{name: "ordinary alias precedes full MCP candidate", tools: []string{"mcp__inventory__read_file"}, suffix: "inventory__read_file", want: "read_file"},
		{name: "ambiguous full MCP names", tools: []string{"mcp__server__tool", "mcp__mcp__server__tool"}, suffix: "mcp__server__tool", wantError: true},
		{name: "ambiguous tool component", tools: []string{"mcp__first__lookup_by_id", "mcp__second__lookup_by_id"}, suffix: "lookup_by_id", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tools := `{"name":"read_file","input_schema":{"type":"object"}}`
			for _, name := range tt.tools {
				tools += fmt.Sprintf(`,{"name":%q,"input_schema":{"type":"object"}}`, name)
			}
			upstream, reverseMap := prepareClaudeOAuthToolNamesForUpstream([]byte(`{"tools":[`+tools+`]}`), claudeMCPAliasOptions{secret: "hybrid-alias-caller"})
			server := claudeMCPAliasServer(gjson.GetBytes(upstream, "tools.0.name").String())
			name := "mcp__" + server + "__" + tt.suffix
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
					block := fmt.Sprintf(`{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}`, name)
					payload := []byte(`{"content":[` + block + `]}`)
					path := "content.0.name"
					restore := restoreClaudeOAuthToolNamesFromResponse
					if stream {
						payload = []byte(`data: {"type":"content_block_start","index":0,"content_block":` + block + `}`)
						path = "content_block.name"
						restore = restoreClaudeOAuthToolNamesFromStreamLine
					}
					restored, err := restore(payload, reverseMap)
					if tt.wantError {
						if err == nil || !strings.Contains(err.Error(), "multiple") {
							t.Fatalf("ambiguous name error = %v, want multiple matches", err)
						}
						return
					}
					if err != nil {
						t.Fatalf("restore tool name: %v", err)
					}
					if got := gjson.GetBytes(bytes.TrimPrefix(restored, []byte("data: ")), path).String(); got != tt.want {
						t.Fatalf("tool name = %q, want %q", got, tt.want)
					}
				})
			}
		})
	}
}
