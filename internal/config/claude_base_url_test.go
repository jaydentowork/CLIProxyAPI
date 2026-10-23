package config

import (
	"reflect"
	"testing"
)

func TestParseConfigBytesIgnoresLegacyGlobalBaseURLs(t *testing.T) {
	const credentials = "claude-api-key:\n  - api-key: claude-key\n    base-url: https://claude.credential.example\ncodex-api-key:\n  - api-key: codex-key\n    base-url: https://codex.credential.example\n"
	baseline, err := ParseConfigBytes([]byte(credentials))
	if err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []string{
		"claude-base-url: https://legacy.claude.example\ncodex-base-url: https://legacy.codex.example\n",
		"config-version: 8\nupstream:\n  claude:\n    base-url: https://legacy.claude.example\noauth:\n  providers:\n    codex:\n      base-url: https://legacy.codex.example\n",
		"config-version: 8\noauth:\n  providers:\n    claude:\n      base-url: https://legacy.claude.example\n",
	} {
		cfg, errParse := ParseConfigBytes([]byte(legacy + credentials))
		if errParse != nil {
			t.Fatal(errParse)
		}
		if !reflect.DeepEqual(cfg, baseline) {
			t.Fatal("legacy global base URL fields changed parsed config")
		}
		if len(cfg.ClaudeKey) != 1 || cfg.ClaudeKey[0].BaseURL != "https://claude.credential.example" || len(cfg.CodexKey) != 1 || cfg.CodexKey[0].BaseURL != "https://codex.credential.example" {
			t.Fatal("per-credential base URLs were not preserved")
		}
	}
}
