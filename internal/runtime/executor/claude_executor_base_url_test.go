package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
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

func TestClaudeCredentialBaseURLIgnoresLegacyEnvironment(t *testing.T) {
	t.Setenv("CLAUDE_BASE_URL", "https://legacy.example.com")
	for _, auth := range []*cliproxyauth.Auth{newClaudeOAuthTestAuth(), newClaudeAPIKeyTestAuth("key", "")} {
		_, baseURL := claudeCreds(auth)
		if baseURL != "" {
			t.Fatalf("legacy environment changed credential base URL: %q", baseURL)
		}
		auth.Attributes = map[string]string{"base_url": "https://credential.example.com"}
		_, baseURL = claudeCreds(auth)
		if baseURL != "https://credential.example.com" {
			t.Fatalf("credential base URL = %q", baseURL)
		}
	}
}

func TestClaudeExecutorBaseURLSelection(t *testing.T) {
	t.Setenv("CLAUDE_BASE_URL", "https://legacy.example.com")
	for _, method := range []string{"execute", "stream", "count_tokens", "count_tokens_upstream"} {
		for _, baseURL := range []string{"", "  https://api.anthropic.com///  "} {
			t.Run(method+"/"+baseURL, func(t *testing.T) {
				var gotURL string
				transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					gotURL = req.URL.String()
					return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"test rejection"}}`)), Request: req}, nil
				})
				ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
				auth := newClaudeOAuthTestAuth()
				auth.Metadata["account_uuid"] = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
				auth.Attributes = map[string]string{"base_url": baseURL}
				ex := NewClaudeExecutor(&config.Config{})
				req := cliproxyexecutor.Request{Model: "claude-sonnet-4-5", Payload: []byte(`{"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
				var err error
				switch method {
				case "execute":
					_, err = ex.Execute(ctx, auth, req, opts)
				case "stream":
					_, err = ex.ExecuteStream(ctx, auth, req, opts)
				case "count_tokens":
					_, err = ex.CountTokens(ctx, auth, req, opts)
				case "count_tokens_upstream":
					_, err = ex.countTokensUpstream(ctx, auth, req, opts)
				}
				wantURL := "https://api.anthropic.com/v1/messages?beta=true"
				if strings.HasPrefix(method, "count_tokens") {
					wantURL = "https://api.anthropic.com/v1/messages/count_tokens?beta=true"
				}
				if gotURL != wantURL {
					t.Fatalf("outbound URL = %q, want %q (error: %v)", gotURL, wantURL, err)
				}
			})
		}
	}
}
