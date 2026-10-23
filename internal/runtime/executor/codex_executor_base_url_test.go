package executor

import (
	"context"
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
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

func TestCodexCredentialBaseURLIgnoresLegacyEnvironment(t *testing.T) {
	t.Setenv("CODEX_BASE_URL", "https://legacy.example.com")
	for _, auth := range []*cliproxyauth.Auth{newCodexOAuthTestAuth(), newCodexAPIKeyTestAuth("key", "")} {
		_, baseURL := codexCredsForRequest(context.Background(), auth)
		if baseURL != "" {
			t.Fatalf("legacy environment changed credential base URL: %q", baseURL)
		}
		auth.Attributes = map[string]string{"base_url": "https://credential.example.com"}
		_, baseURL = codexCredsForRequest(context.Background(), auth)
		if baseURL != "https://credential.example.com" {
			t.Fatalf("credential base URL = %q", baseURL)
		}
	}
}

func TestCodexSIWCBaseURLComposition(t *testing.T) {
	t.Setenv("CODEX_BASE_URL", "https://legacy.example.com")
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": "outer-token",
			"use_siwc":     true,
			"siwc":         map[string]any{"access_token": "siwc-token"},
		},
	}
	for _, baseURL := range []string{"", "https://credential.example.com"} {
		auth.Attributes = map[string]string{"base_url": baseURL}
		key, got := codexCredsForRequest(context.Background(), auth)
		want := baseURL
		if want == "" {
			want = defaultOpenAISIWCBaseURL
		}
		if key != "siwc-token" || got != want {
			t.Fatalf("SIWC credentials = (%q, %q), want SIWC token and %q", key, got, want)
		}
		headers := http.Header{"User-Agent": []string{"codex_cli_rs/0.1.0"}}
		key, got = codexCredsForRequest(context.Background(), auth, headers)
		if key != "outer-token" || got != baseURL {
			t.Fatalf("request override credentials = (%q, %q), want outer token and %q", key, got, baseURL)
		}
	}
}
