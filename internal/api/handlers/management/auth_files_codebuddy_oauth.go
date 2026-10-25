package management

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// codeBuddyLoginProvider is the OAuth session provider key for CodeBuddy logins.
const codeBuddyLoginProvider = "codebuddy"

// RequestCodeBuddyToken starts the CodeBuddy browser login flow. It returns the
// authorization URL immediately and saves the credential once the browser
// handshake issues a token and account.
func (h *Handler) RequestCodeBuddyToken(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "configuration is not available"})
		return
	}
	cfg := h.configSnapshot()
	if cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "configuration is not available"})
		return
	}

	// The login outlives the HTTP request, so it must not inherit its cancellation.
	ctx := PopulateAuthContext(context.Background(), c)
	endpoint := codebuddy.NormalizeEndpoint(codebuddy.DefaultEndpoint)
	client := codebuddy.NewLoginClient(util.SetProxy(&cfg.SDKConfig, &http.Client{Timeout: 30 * time.Second}), endpoint)

	fmt.Println("Initializing CodeBuddy authentication...")
	authURL, upstreamState, errStart := client.StartLogin(ctx)
	if errStart != nil {
		log.Errorf("Failed to generate CodeBuddy authorization URL: %v", errStart)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}

	state := fmt.Sprintf("cbd-%d", time.Now().UnixNano())
	RegisterOAuthSession(state, codeBuddyLoginProvider)

	go h.completeCodeBuddyLogin(ctx, state, upstreamState, endpoint, client)

	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": authURL, "state": state})
}

// completeCodeBuddyLogin polls until the browser login completes and persists the
// resulting credential. The session stops as soon as it is cancelled or completed.
func (h *Handler) completeCodeBuddyLogin(ctx context.Context, state, upstreamState, endpoint string, client *codebuddy.LoginClient) {
	pollCtx, cancelPoll := context.WithCancel(ctx)
	defer cancelPoll()
	go watchOAuthSessionCancel(pollCtx, cancelPoll, state, codeBuddyLoginProvider)

	fmt.Println("Waiting for CodeBuddy authentication...")
	token, account, errWait := client.WaitForLogin(pollCtx, upstreamState)
	if errWait != nil {
		if !IsOAuthSessionPending(state, codeBuddyLoginProvider) {
			return
		}
		SetOAuthSessionError(state, oauthSessionErrorWithCause("Authentication failed", errWait))
		fmt.Printf("CodeBuddy authentication failed: %v\n", errWait)
		return
	}
	if !IsOAuthSessionPending(state, codeBuddyLoginProvider) {
		return
	}

	machineID := codebuddy.NewMachineID()
	metadata := codebuddy.BuildMetadata(token, account, machineID, endpoint)
	identity := codebuddy.IdentityFromMetadata(metadata, nil)
	fileName := fmt.Sprintf("%s-%d.json", codebuddy.Provider, time.Now().UnixMilli())
	record := &coreauth.Auth{
		ID:       fileName,
		Provider: codebuddy.Provider,
		FileName: fileName,
		Label:    "CodeBuddy " + account.DisplayName(),
		Storage:  codebuddy.NewTokenStorage(token, account, machineID, endpoint),
		Metadata: metadata,
		Attributes: map[string]string{
			"base_url": endpoint,
			"domain":   identity.DomainOrDefault(),
		},
	}

	if errGuard := guardOAuthSessionPendingForSave(state, codeBuddyLoginProvider); errGuard != nil {
		return
	}
	savedPath, errSave := h.saveTokenRecord(ctx, record)
	if errSave != nil {
		log.Errorf("Failed to save CodeBuddy authentication tokens: %v", errSave)
		SetOAuthSessionError(state, "Failed to save authentication tokens")
		return
	}

	fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
	CompleteOAuthSession(state)
}
