package config

import "testing"

func TestParseConfigBytesClaudeBaseURL(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "defaults to empty when omitted",
			yaml: "port: 8317\n",
			want: "",
		},
		{
			name: "parses claude-base-url when configured",
			yaml: "claude-base-url: \"http://127.0.0.1:8080\"\n",
			want: "http://127.0.0.1:8080",
		},
		{
			name: "parses claude-base-url with trailing slash preserved at config parse",
			yaml: "claude-base-url: \"https://custom.anthropic.proxy/v1/\"\n",
			want: "https://custom.anthropic.proxy/v1/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(tt.yaml))
			if errParse != nil {
				t.Fatalf("ParseConfigBytes() error = %v", errParse)
			}
			if got := cfg.ClaudeBaseURL; got != tt.want {
				t.Fatalf("ClaudeBaseURL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseConfigBytesCodexBaseURL(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "defaults to empty when omitted",
			yaml: "port: 8317\n",
			want: "",
		},
		{
			name: "parses codex-base-url when configured",
			yaml: "codex-base-url: \"http://127.0.0.1:9090\"\n",
			want: "http://127.0.0.1:9090",
		},
		{
			name: "parses codex-base-url with trailing slash preserved at config parse",
			yaml: "codex-base-url: \"https://custom.openai.proxy/backend-api/codex/\"\n",
			want: "https://custom.openai.proxy/backend-api/codex/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(tt.yaml))
			if errParse != nil {
				t.Fatalf("ParseConfigBytes() error = %v", errParse)
			}
			if got := cfg.CodexBaseURL; got != tt.want {
				t.Fatalf("CodexBaseURL = %q, want %q", got, tt.want)
			}
		})
	}
}
