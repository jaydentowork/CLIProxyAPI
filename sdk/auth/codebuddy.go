package auth

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// codeBuddyRefreshLead is the duration before token expiry when refresh should occur.
var codeBuddyRefreshLead = 5 * time.Minute

// CodeBuddyAuthenticator implements the browser login flow for Tencent CodeBuddy
// (global edition).
type CodeBuddyAuthenticator struct{}

// NewCodeBuddyAuthenticator constructs a new CodeBuddy authenticator.
func NewCodeBuddyAuthenticator() Authenticator {
	return &CodeBuddyAuthenticator{}
}

// Provider returns the provider key for CodeBuddy.
func (CodeBuddyAuthenticator) Provider() string {
	return codebuddy.Provider
}

// RefreshLead returns the duration before token expiry when refresh should occur.
func (CodeBuddyAuthenticator) RefreshLead() *time.Duration {
	return &codeBuddyRefreshLead
}

// Login runs the CodeBuddy browser login: it asks the gateway for an auth URL,
// opens it, then polls until the browser handshake issues a token and account.
func (CodeBuddyAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	endpoint := codebuddy.NormalizeEndpoint(codebuddy.DefaultEndpoint)
	client := codebuddy.NewLoginClient(util.SetProxy(&cfg.SDKConfig, &http.Client{Timeout: 30 * time.Second}), endpoint)

	fmt.Println("Starting CodeBuddy authentication...")
	authURL, state, errStart := client.StartLogin(ctx)
	if errStart != nil {
		return nil, fmt.Errorf("codebuddy: %w", errStart)
	}

	fmt.Printf("\nTo authenticate, please visit:\n%s\n\n", authURL)
	if !opts.NoBrowser && browser.IsAvailable() {
		if errOpen := browser.OpenURL(authURL); errOpen != nil {
			log.Warnf("Failed to open browser automatically: %v", errOpen)
		} else {
			fmt.Println("Browser opened automatically.")
		}
	}

	fmt.Println("Waiting for authorization...")
	token, account, errWait := client.WaitForLogin(ctx, state)
	if errWait != nil {
		return nil, fmt.Errorf("codebuddy: %w", errWait)
	}

	machineID := codebuddy.NewMachineID()
	metadata := codebuddy.BuildMetadata(token, account, machineID, endpoint)
	identity := codebuddy.IdentityFromMetadata(metadata, nil)
	storage := codebuddy.NewTokenStorage(token, account, machineID, endpoint)

	displayName := account.DisplayName()
	fileName := fmt.Sprintf("%s-%d.json", codebuddy.Provider, time.Now().UnixMilli())

	fmt.Printf("\nCodeBuddy authentication successful!\n")

	return &coreauth.Auth{
		ID:       fileName,
		Provider: codebuddy.Provider,
		FileName: fileName,
		Label:    "CodeBuddy " + displayName,
		Storage:  storage,
		Metadata: metadata,
		Attributes: map[string]string{
			"base_url": endpoint,
			"domain":   identity.DomainOrDefault(),
		},
	}, nil
}
