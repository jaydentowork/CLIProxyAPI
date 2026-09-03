package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func newCodexOAuthTestAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		Metadata: map[string]any{
			"access_token": "valid-codex-oauth-token",
		},
	}
}

func newCodexAPIKeyTestAuth(key, baseURL string) *cliproxyauth.Auth {
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

func TestCodexCredentialUsesOAuth(t *testing.T) {
	tests := []struct {
		name string
		auth *cliproxyauth.Auth
		want bool
	}{
		{
			name: "nil auth returns false",
			auth: nil,
			want: false,
		},
		{
			name: "AuthKindAPIKey returns false",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindAPIKey,
				},
				Metadata: map[string]any{
					"access_token": "some-token",
				},
			},
			want: false,
		},
		{
			name: "api_key attribute set returns false",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"api_key": "sk-codex-api-key",
				},
			},
			want: false,
		},
		{
			name: "api_key attribute empty and access_token metadata present returns true",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"api_key": "   ",
				},
				Metadata: map[string]any{
					"access_token": "some-token",
				},
			},
			want: true,
		},
		{
			name: "access_token metadata only returns true",
			auth: newCodexOAuthTestAuth(),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := codexCredentialUsesOAuth(tt.auth)
			if got != tt.want {
				t.Fatalf("codexCredentialUsesOAuth() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCodexExecutorResolveCodexBaseURL(t *testing.T) {
	oauthAuth := newCodexOAuthTestAuth()

	// (a) Credential base URL wins over CODEX_BASE_URL env and codex-base-url config
	t.Run("credential base URL wins over env and config", func(t *testing.T) {
		t.Setenv("CODEX_BASE_URL", "https://env.example.com")
		e := &CodexExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					CodexBaseURL: "https://cfg.example.com",
				},
			},
		}
		got := e.resolveCodexBaseURL(oauthAuth, "https://cred.example.com")
		if got != "https://cred.example.com" {
			t.Fatalf("resolveCodexBaseURL() = %q, want %q", got, "https://cred.example.com")
		}
	})

	// (b) CODEX_BASE_URL env wins over codex-base-url config when credential base URL is empty (OAuth)
	t.Run("env wins over config", func(t *testing.T) {
		t.Setenv("CODEX_BASE_URL", "https://env.example.com")
		e := &CodexExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					CodexBaseURL: "https://cfg.example.com",
				},
			},
		}
		got := e.resolveCodexBaseURL(oauthAuth, "")
		if got != "https://env.example.com" {
			t.Fatalf("resolveCodexBaseURL() = %q, want %q", got, "https://env.example.com")
		}
	})

	// (c) codex-base-url config used when env unset and credential empty (OAuth)
	t.Run("config used when env unset and credential empty", func(t *testing.T) {
		t.Setenv("CODEX_BASE_URL", "")
		e := &CodexExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					CodexBaseURL: "https://cfg.example.com",
				},
			},
		}
		got := e.resolveCodexBaseURL(oauthAuth, "")
		if got != "https://cfg.example.com" {
			t.Fatalf("resolveCodexBaseURL() = %q, want %q", got, "https://cfg.example.com")
		}
	})

	// (d) nil cfg + empty everything yields default https://chatgpt.com/backend-api/codex
	t.Run("nil cfg and empty everything yields default", func(t *testing.T) {
		t.Setenv("CODEX_BASE_URL", "")
		e := &CodexExecutor{cfg: nil}
		got := e.resolveCodexBaseURL(oauthAuth, "")
		if got != "https://chatgpt.com/backend-api/codex" {
			t.Fatalf("resolveCodexBaseURL() = %q, want %q", got, "https://chatgpt.com/backend-api/codex")
		}
	})

	t.Run("empty config and empty everything yields default", func(t *testing.T) {
		t.Setenv("CODEX_BASE_URL", "")
		e := &CodexExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					CodexBaseURL: "",
				},
			},
		}
		got := e.resolveCodexBaseURL(oauthAuth, "")
		if got != "https://chatgpt.com/backend-api/codex" {
			t.Fatalf("resolveCodexBaseURL() = %q, want %q", got, "https://chatgpt.com/backend-api/codex")
		}
	})

	// (e) Whitespace and trailing-slash trimming
	t.Run("whitespace and trailing slash trimming on credential", func(t *testing.T) {
		t.Setenv("CODEX_BASE_URL", "https://env.example.com")
		e := &CodexExecutor{}
		got := e.resolveCodexBaseURL(oauthAuth, "  https://cred.example.com/  ")
		if got != "https://cred.example.com" {
			t.Fatalf("resolveCodexBaseURL() = %q, want %q", got, "https://cred.example.com")
		}
	})

	t.Run("whitespace and trailing slash trimming on env", func(t *testing.T) {
		t.Setenv("CODEX_BASE_URL", "  https://env.example.com/  ")
		e := &CodexExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					CodexBaseURL: "https://cfg.example.com",
				},
			},
		}
		got := e.resolveCodexBaseURL(oauthAuth, "")
		if got != "https://env.example.com" {
			t.Fatalf("resolveCodexBaseURL() = %q, want %q", got, "https://env.example.com")
		}
	})

	t.Run("whitespace and trailing slash trimming on config", func(t *testing.T) {
		t.Setenv("CODEX_BASE_URL", "")
		e := &CodexExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					CodexBaseURL: "  https://cfg.example.com/  ",
				},
			},
		}
		got := e.resolveCodexBaseURL(oauthAuth, "")
		if got != "https://cfg.example.com" {
			t.Fatalf("resolveCodexBaseURL() = %q, want %q", got, "https://cfg.example.com")
		}
	})

	t.Run("whitespace-only env treated as unset", func(t *testing.T) {
		t.Setenv("CODEX_BASE_URL", "   ")
		e := &CodexExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					CodexBaseURL: "https://cfg.example.com",
				},
			},
		}
		got := e.resolveCodexBaseURL(oauthAuth, "")
		if got != "https://cfg.example.com" {
			t.Fatalf("resolveCodexBaseURL() = %q, want %q", got, "https://cfg.example.com")
		}
	})

	// (f) API-key auth with empty base URL and env+config set -> returns default https://chatgpt.com/backend-api/codex
	t.Run("api key credential ignores env and config overrides", func(t *testing.T) {
		t.Setenv("CODEX_BASE_URL", "https://env.example.com")
		apiKeyAuth := newCodexAPIKeyTestAuth("sk-codex-api-key", "")
		e := &CodexExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					CodexBaseURL: "https://cfg.example.com",
				},
			},
		}
		got := e.resolveCodexBaseURL(apiKeyAuth, "")
		if got != "https://chatgpt.com/backend-api/codex" {
			t.Fatalf("resolveCodexBaseURL() = %q, want default %q", got, "https://chatgpt.com/backend-api/codex")
		}
	})

	// (g) API-key auth WITH per-credential base URL -> returns that credential URL
	t.Run("api key credential with custom base url preserves it", func(t *testing.T) {
		t.Setenv("CODEX_BASE_URL", "https://env.example.com")
		apiKeyAuth := newCodexAPIKeyTestAuth("sk-codex-api-key", "https://custom.gateway.example.com")
		e := &CodexExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					CodexBaseURL: "https://cfg.example.com",
				},
			},
		}
		got := e.resolveCodexBaseURL(apiKeyAuth, "https://custom.gateway.example.com")
		if got != "https://custom.gateway.example.com" {
			t.Fatalf("resolveCodexBaseURL() = %q, want %q", got, "https://custom.gateway.example.com")
		}
	})

	// Full precedence progression
	t.Run("full precedence progression", func(t *testing.T) {
		t.Setenv("CODEX_BASE_URL", "")
		e := &CodexExecutor{
			cfg: &config.Config{
				SDKConfig: config.SDKConfig{
					CodexBaseURL: "https://cfg.example/",
				},
			},
		}
		if got := e.resolveCodexBaseURL(oauthAuth, ""); got != "https://cfg.example" {
			t.Fatalf("step 1 (config with trailing slash): got %q, want %q", got, "https://cfg.example")
		}

		t.Setenv("CODEX_BASE_URL", "https://env.example")
		if got := e.resolveCodexBaseURL(oauthAuth, ""); got != "https://env.example" {
			t.Fatalf("step 2 (env wins over config): got %q, want %q", got, "https://env.example")
		}

		if got := e.resolveCodexBaseURL(oauthAuth, "https://cred.example"); got != "https://cred.example" {
			t.Fatalf("step 3 (credential wins over env): got %q, want %q", got, "https://cred.example")
		}

		t.Setenv("CODEX_BASE_URL", "")
		nilExec := &CodexExecutor{cfg: nil}
		if got := nilExec.resolveCodexBaseURL(oauthAuth, ""); got != "https://chatgpt.com/backend-api/codex" {
			t.Fatalf("step 4 (nil cfg + empty env + empty cred): got %q, want %q", got, "https://chatgpt.com/backend-api/codex")
		}
	})
}
