package executor

import (
	"os"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// CodexExecutor is a stateless executor for Codex (OpenAI Responses API entrypoint).
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type CodexExecutor struct {
	cfg *config.Config
}

func NewCodexExecutor(cfg *config.Config) *CodexExecutor { return &CodexExecutor{cfg: cfg} }

func (e *CodexExecutor) Identifier() string { return "codex" }

const defaultCodexBaseURL = "https://chatgpt.com/backend-api/codex"

// resolveCodexBaseURL picks the upstream Codex endpoint. A credential's own
// base URL (codex-api-key base-url) always wins. Only OAuth credentials fall
// through to the CODEX_BASE_URL env var and then the codex-base-url config
// field: OAuth pools carry no per-credential knob, while an API key that set no
// base-url intentionally targets the default endpoint and must never be redirected by a
// global proxy setting. Empty everywhere keeps the default Codex origin.
func (e *CodexExecutor) resolveCodexBaseURL(auth *cliproxyauth.Auth, credentialBaseURL string) string {
	if cred := strings.TrimRight(strings.TrimSpace(credentialBaseURL), "/"); cred != "" {
		return cred
	}
	if !codexCredentialUsesOAuth(auth) {
		return defaultCodexBaseURL
	}
	if env := strings.TrimRight(strings.TrimSpace(os.Getenv("CODEX_BASE_URL")), "/"); env != "" {
		return env
	}
	if e != nil && e.cfg != nil {
		if cfgURL := strings.TrimRight(strings.TrimSpace(e.cfg.CodexBaseURL), "/"); cfgURL != "" {
			return cfgURL
		}
	}
	return defaultCodexBaseURL
}

func (e *CodexExecutor) modelLevelCooling() bool {
	return e != nil && e.cfg != nil && e.cfg.Codex.ModelLevelCooling
}
