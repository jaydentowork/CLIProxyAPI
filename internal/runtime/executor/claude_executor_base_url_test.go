package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func newClaudeOAuthTestAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		Metadata: map[string]any{
			"access_token": "sk-ant-oat01-valid-oauth-token",
		},
	}
}

func newClaudeAPIKeyTestAuth(key, baseURL string) *cliproxyauth.Auth {
	attrs := map[string]string{
		"api_key": key,
	}
	if baseURL != "" {
		attrs["base_url"] = baseURL
	}
	return &cliproxyauth.Auth{
		Attributes: attrs,
	}
}

func TestClaudeExecutorResolveClaudeBaseURL(t *testing.T) {
	oauthAuth := newClaudeOAuthTestAuth()
	oauthKey := "sk-ant-oat01-valid-oauth-token"

	// (a) Credential base URL wins over CLAUDE_BASE_URL env and claude-base-url config
	t.Run("credential base URL wins over env and config", func(t *testing.T) {
		t.Setenv("CLAUDE_BASE_URL", "https://env.example.com")
		e := &ClaudeExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					ClaudeBaseURL: "https://cfg.example.com",
				},
			},
		}
		got := e.resolveClaudeBaseURL(oauthAuth, oauthKey, "https://cred.example.com")
		if got != "https://cred.example.com" {
			t.Fatalf("resolveClaudeBaseURL() = %q, want %q", got, "https://cred.example.com")
		}
	})

	// (b) CLAUDE_BASE_URL env wins over claude-base-url config when credential base URL is empty (OAuth)
	t.Run("env wins over config", func(t *testing.T) {
		t.Setenv("CLAUDE_BASE_URL", "https://env.example.com")
		e := &ClaudeExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					ClaudeBaseURL: "https://cfg.example.com",
				},
			},
		}
		got := e.resolveClaudeBaseURL(oauthAuth, oauthKey, "")
		if got != "https://env.example.com" {
			t.Fatalf("resolveClaudeBaseURL() = %q, want %q", got, "https://env.example.com")
		}
	})

	// (c) claude-base-url config used when env unset and credential empty (OAuth)
	t.Run("config used when env unset and credential empty", func(t *testing.T) {
		t.Setenv("CLAUDE_BASE_URL", "")
		e := &ClaudeExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					ClaudeBaseURL: "https://cfg.example.com",
				},
			},
		}
		got := e.resolveClaudeBaseURL(oauthAuth, oauthKey, "")
		if got != "https://cfg.example.com" {
			t.Fatalf("resolveClaudeBaseURL() = %q, want %q", got, "https://cfg.example.com")
		}
	})

	// (d) nil cfg + empty everything yields default https://api.anthropic.com
	t.Run("nil cfg and empty everything yields default", func(t *testing.T) {
		t.Setenv("CLAUDE_BASE_URL", "")
		e := &ClaudeExecutor{cfg: nil}
		got := e.resolveClaudeBaseURL(oauthAuth, oauthKey, "")
		if got != "https://api.anthropic.com" {
			t.Fatalf("resolveClaudeBaseURL() = %q, want %q", got, "https://api.anthropic.com")
		}
	})

	t.Run("empty config and empty everything yields default", func(t *testing.T) {
		t.Setenv("CLAUDE_BASE_URL", "")
		e := &ClaudeExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					ClaudeBaseURL: "",
				},
			},
		}
		got := e.resolveClaudeBaseURL(oauthAuth, oauthKey, "")
		if got != "https://api.anthropic.com" {
			t.Fatalf("resolveClaudeBaseURL() = %q, want %q", got, "https://api.anthropic.com")
		}
	})

	// (e) Whitespace and trailing-slash trimming
	t.Run("whitespace and trailing slash trimming on credential", func(t *testing.T) {
		t.Setenv("CLAUDE_BASE_URL", "https://env.example.com")
		e := &ClaudeExecutor{}
		got := e.resolveClaudeBaseURL(oauthAuth, oauthKey, "  https://cred.example.com/  ")
		if got != "https://cred.example.com" {
			t.Fatalf("resolveClaudeBaseURL() = %q, want %q", got, "https://cred.example.com")
		}
	})

	t.Run("whitespace and trailing slash trimming on env", func(t *testing.T) {
		t.Setenv("CLAUDE_BASE_URL", "  https://env.example.com/  ")
		e := &ClaudeExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					ClaudeBaseURL: "https://cfg.example.com",
				},
			},
		}
		got := e.resolveClaudeBaseURL(oauthAuth, oauthKey, "")
		if got != "https://env.example.com" {
			t.Fatalf("resolveClaudeBaseURL() = %q, want %q", got, "https://env.example.com")
		}
	})

	t.Run("whitespace and trailing slash trimming on config", func(t *testing.T) {
		t.Setenv("CLAUDE_BASE_URL", "")
		e := &ClaudeExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					ClaudeBaseURL: "  https://cfg.example.com/  ",
				},
			},
		}
		got := e.resolveClaudeBaseURL(oauthAuth, oauthKey, "")
		if got != "https://cfg.example.com" {
			t.Fatalf("resolveClaudeBaseURL() = %q, want %q", got, "https://cfg.example.com")
		}
	})

	t.Run("whitespace-only env treated as unset", func(t *testing.T) {
		t.Setenv("CLAUDE_BASE_URL", "   ")
		e := &ClaudeExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					ClaudeBaseURL: "https://cfg.example.com",
				},
			},
		}
		got := e.resolveClaudeBaseURL(oauthAuth, oauthKey, "")
		if got != "https://cfg.example.com" {
			t.Fatalf("resolveClaudeBaseURL() = %q, want %q", got, "https://cfg.example.com")
		}
	})

	// (f) API-key auth with empty base URL and env+config set -> returns default https://api.anthropic.com
	t.Run("api key credential ignores env and config overrides", func(t *testing.T) {
		t.Setenv("CLAUDE_BASE_URL", "https://env.example.com")
		apiKeyAuth := newClaudeAPIKeyTestAuth("sk-ant-api03-test-key", "")
		e := &ClaudeExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					ClaudeBaseURL: "https://cfg.example.com",
				},
			},
		}
		got := e.resolveClaudeBaseURL(apiKeyAuth, "sk-ant-api03-test-key", "")
		if got != "https://api.anthropic.com" {
			t.Fatalf("resolveClaudeBaseURL() = %q, want default %q", got, "https://api.anthropic.com")
		}
	})

	// (g) API-key auth WITH per-credential base URL -> returns that credential URL
	t.Run("api key credential with custom base url preserves it", func(t *testing.T) {
		t.Setenv("CLAUDE_BASE_URL", "https://env.example.com")
		apiKeyAuth := newClaudeAPIKeyTestAuth("sk-ant-api03-test-key", "https://custom.gateway.example.com")
		e := &ClaudeExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					ClaudeBaseURL: "https://cfg.example.com",
				},
			},
		}
		got := e.resolveClaudeBaseURL(apiKeyAuth, "sk-ant-api03-test-key", "https://custom.gateway.example.com")
		if got != "https://custom.gateway.example.com" {
			t.Fatalf("resolveClaudeBaseURL() = %q, want %q", got, "https://custom.gateway.example.com")
		}
	})

	// Full precedence progression
	t.Run("full precedence progression", func(t *testing.T) {
		t.Setenv("CLAUDE_BASE_URL", "")
		e := &ClaudeExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					ClaudeBaseURL: "https://cfg.example/",
				},
			},
		}
		if got := e.resolveClaudeBaseURL(oauthAuth, oauthKey, ""); got != "https://cfg.example" {
			t.Fatalf("step 1 (config with trailing slash): got %q, want %q", got, "https://cfg.example")
		}

		t.Setenv("CLAUDE_BASE_URL", "https://env.example")
		if got := e.resolveClaudeBaseURL(oauthAuth, oauthKey, ""); got != "https://env.example" {
			t.Fatalf("step 2 (env wins over config): got %q, want %q", got, "https://env.example")
		}

		if got := e.resolveClaudeBaseURL(oauthAuth, oauthKey, "https://cred.example"); got != "https://cred.example" {
			t.Fatalf("step 3 (credential wins over env): got %q, want %q", got, "https://cred.example")
		}

		t.Setenv("CLAUDE_BASE_URL", "")
		nilExec := &ClaudeExecutor{cfg: nil}
		if got := nilExec.resolveClaudeBaseURL(oauthAuth, oauthKey, ""); got != "https://api.anthropic.com" {
			t.Fatalf("step 4 (nil cfg + empty env + empty cred): got %q, want %q", got, "https://api.anthropic.com")
		}
	})
}
