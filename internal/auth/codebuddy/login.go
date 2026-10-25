package codebuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// Login flow defaults.
const (
	// DefaultPollInterval is how often the token and account endpoints are polled.
	DefaultPollInterval = time.Second
	// DefaultLoginDeadline bounds the whole browser login handshake.
	DefaultLoginDeadline = 5 * time.Minute

	// maxResponseBytes caps how much of an upstream auth response is read.
	maxResponseBytes = 1 << 20
)

// Account is the CodeBuddy login account object.
type Account struct {
	UID            string
	Nickname       string
	EnterpriseID   string
	DepartmentInfo string
	// Raw is the unwrapped upstream account object, kept for diagnostics.
	Raw map[string]any
}

// LoginClient drives the CodeBuddy browser login handshake. The zero value is
// not usable; build one with NewLoginClient.
type LoginClient struct {
	// HTTPClient performs the login requests. Nil means a default client.
	HTTPClient *http.Client
	// Endpoint is the CodeBuddy gateway (see NormalizeEndpoint).
	Endpoint string
	// PollInterval overrides DefaultPollInterval when positive.
	PollInterval time.Duration
	// Deadline overrides DefaultLoginDeadline when positive.
	Deadline time.Duration
}

// NewLoginClient creates a login client for a CodeBuddy endpoint.
func NewLoginClient(httpClient *http.Client, endpoint string) *LoginClient {
	return &LoginClient{
		HTTPClient:   httpClient,
		Endpoint:     NormalizeEndpoint(endpoint),
		PollInterval: DefaultPollInterval,
		Deadline:     DefaultLoginDeadline,
	}
}

func (c *LoginClient) endpoint() string {
	if c == nil {
		return DefaultEndpoint
	}
	return NormalizeEndpoint(c.Endpoint)
}

func (c *LoginClient) httpClient() *http.Client {
	if c != nil && c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *LoginClient) pollInterval() time.Duration {
	if c != nil && c.PollInterval > 0 {
		return c.PollInterval
	}
	return DefaultPollInterval
}

func (c *LoginClient) loginDeadline() time.Duration {
	if c != nil && c.Deadline > 0 {
		return c.Deadline
	}
	return DefaultLoginDeadline
}

// StartLogin requests a login URL and its polling state. The returned URL is
// opened in a browser by the caller; the state is passed to WaitForLogin.
func (c *LoginClient) StartLogin(ctx context.Context) (string, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint := c.endpoint()
	requestURL := BuildURL(endpoint, AuthStatePath) + "?platform=" + url.QueryEscape(LoginPlatform)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", "", fmt.Errorf("codebuddy: build state request: %w", err)
	}
	for key, value := range stateRequestHeaders(EndpointAuthority(endpoint)) {
		req.Header.Set(key, value)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", "", fmt.Errorf("codebuddy: state request: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("codebuddy login: close state response body error: %v", errClose)
		}
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", "", fmt.Errorf("codebuddy: read state response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	obj, err := decodeObject(body)
	if err != nil {
		return "", "", err
	}
	authURL := metaString(obj, "authUrl")
	state := metaString(obj, "state")
	if authURL == "" || state == "" {
		return "", "", fmt.Errorf("codebuddy: state response missing authUrl or state")
	}
	return authURL, state, nil
}

// WaitForLogin polls the token endpoint until an access token is issued and the
// account endpoint until the account carries a uid. Both polls share one
// deadline: the flow deadline of the first call to WaitForLogin.
func (c *LoginClient) WaitForLogin(ctx context.Context, state string) (*TokenResult, *Account, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	state = strings.TrimSpace(state)
	if state == "" {
		return nil, nil, fmt.Errorf("codebuddy: login state is required")
	}
	endpoint := c.endpoint()
	deadline := time.Now().Add(c.loginDeadline())
	escapedState := url.QueryEscape(state)

	var token *TokenResult
	tokenURL := BuildURL(endpoint, AuthTokenPath) + "?state=" + escapedState
	tokenHeaders := stateRequestHeaders(EndpointAuthority(endpoint))
	if err := c.pollUntil(ctx, deadline, false, func(ctx context.Context) (bool, error) {
		obj, errPoll := c.getObject(ctx, tokenURL, tokenHeaders)
		if errPoll != nil {
			// Pending logins answer with transport or HTTP errors; keep polling.
			return false, nil
		}
		token = ParseTokenObject(obj, time.Now())
		return token != nil, nil
	}); err != nil {
		return nil, nil, err
	}

	var account *Account
	accountURL := BuildURL(endpoint, AccountPath) + "?state=" + escapedState
	identity := Identity{Endpoint: endpoint, Domain: token.Domain}
	accountHeaders := accountRequestHeaders(identity.DomainOrDefault(), token.AccessToken)
	// The token phase may already have consumed most of the deadline, so poll the
	// account endpoint immediately instead of waiting for the first interval.
	if err := c.pollUntil(ctx, deadline, true, func(ctx context.Context) (bool, error) {
		obj, errPoll := c.getObject(ctx, accountURL, accountHeaders)
		if errPoll != nil {
			return false, nil
		}
		account = ParseAccount(obj)
		return account != nil, nil
	}); err != nil {
		return nil, nil, err
	}
	return token, account, nil
}

// pollUntil runs attempt every PollInterval until it reports done, the context is
// cancelled, or the shared deadline passes. Both the wait between attempts and the
// attempt itself are bounded by the deadline. An attempt error aborts the poll.
//
// firstImmediate skips the wait before the first attempt, so a caller that already
// spent time in an earlier phase can poll right away.
func (c *LoginClient) pollUntil(ctx context.Context, deadline time.Time, firstImmediate bool, attempt func(context.Context) (bool, error)) error {
	firstAttempt := true
	for {
		if errCtx := ctx.Err(); errCtx != nil {
			return fmt.Errorf("codebuddy: login cancelled: %w", errCtx)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("codebuddy: login timed out after %s", c.loginDeadline())
		}
		if !firstImmediate || !firstAttempt {
			wait := c.pollInterval()
			if remaining < wait {
				wait = remaining
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("codebuddy: login cancelled: %w", ctx.Err())
			case <-timer.C:
			}
		}
		firstAttempt = false

		// Never run an attempt with an already-expired context: waiting for the next
		// interval can consume the shared deadline.
		if time.Until(deadline) <= 0 {
			return fmt.Errorf("codebuddy: login timed out after %s", c.loginDeadline())
		}

		// The attempt must not outlive the shared deadline.
		attemptCtx, cancelAttempt := context.WithDeadline(ctx, deadline)
		done, errAttempt := attempt(attemptCtx)
		cancelAttempt()
		if errAttempt != nil {
			return errAttempt
		}
		if done {
			return nil
		}
	}
}

// getObject performs a GET request and returns the unwrapped JSON object.
func (c *LoginClient) getObject(ctx context.Context, rawURL string, headers map[string]string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("codebuddy login: close poll response body error: %v", errClose)
		}
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	return decodeObject(body)
}

// decodeObject unmarshals a response body and peels the data envelope.
func decodeObject(body []byte) (map[string]any, error) {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("codebuddy: decode response: %w", err)
	}
	obj, _ := UnwrapEnvelope(payload).(map[string]any)
	return obj, nil
}

// ParseAccount converts an unwrapped account object into an Account. It returns
// nil while the login is still pending, which CodeBuddy reports as a missing uid.
func ParseAccount(obj map[string]any) *Account {
	if obj == nil {
		return nil
	}
	uid := metaString(obj, "uid")
	if uid == "" {
		return nil
	}
	return &Account{
		UID:            uid,
		Nickname:       metaString(obj, "nickname"),
		EnterpriseID:   metaString(obj, "enterpriseId"),
		DepartmentInfo: metaString(obj, "departmentInfo"),
		Raw:            obj,
	}
}

// DisplayName returns the account name shown in credential labels, preferring the
// nickname over the uid.
func (a *Account) DisplayName() string {
	if a == nil {
		return "User"
	}
	if name := strings.TrimSpace(a.Nickname); name != "" {
		return name
	}
	if uid := strings.TrimSpace(a.UID); uid != "" {
		return uid
	}
	return "User"
}

// stateRequestHeaders returns the headers used before an access token exists:
// the X-No-* identity suppressions plus the X-Domain hint.
func stateRequestHeaders(domain string) map[string]string {
	headers := baseLoginHeaders()
	headers["X-No-Authorization"] = "true"
	headers["X-No-User-Id"] = "true"
	headers["X-No-Enterprise-Id"] = "true"
	headers["X-No-Department-Info"] = "true"
	if domain != "" {
		headers["X-Domain"] = domain
	}
	return headers
}

// accountRequestHeaders returns the headers used after the access token is
// issued. The account endpoint is authenticated, so X-No-Authorization is absent.
func accountRequestHeaders(domain, accessToken string) map[string]string {
	headers := baseLoginHeaders()
	headers["X-No-User-Id"] = "true"
	headers["X-No-Enterprise-Id"] = "true"
	headers["X-No-Department-Info"] = "true"
	if domain != "" {
		headers["X-Domain"] = domain
	}
	if accessToken != "" {
		headers["Authorization"] = "Bearer " + accessToken
	}
	return headers
}

func baseLoginHeaders() map[string]string {
	return map[string]string{
		"User-Agent":     UserAgent,
		"X-Product-Code": ProductCode,
		"Accept":         "application/json",
	}
}

// NewMachineID returns a fresh machine identifier. Callers persist it once per
// credential so refresh and chat requests keep the same identity.
func NewMachineID() string {
	return uuid.New().String()
}

// BuildMetadata assembles the auth record metadata for a completed login. Keys
// mirror the Meta* constants so IdentityFromMetadata can rebuild the identity
// from the persisted auth file.
func BuildMetadata(token *TokenResult, account *Account, machineID, endpoint string) map[string]any {
	endpoint = NormalizeEndpoint(endpoint)
	meta := map[string]any{
		MetaType:        Provider,
		MetaEndpoint:    endpoint,
		MetaMachineID:   strings.TrimSpace(machineID),
		MetaLastRefresh: time.Now().UTC().Format(time.RFC3339),
	}
	tokenDomain := ""
	if token != nil {
		tokenDomain = token.Domain
		meta[MetaAccessToken] = token.AccessToken
		if token.RefreshToken != "" {
			meta[MetaRefreshToken] = token.RefreshToken
		}
		if token.ExpiresAtMS > 0 {
			meta[MetaExpiresAt] = token.ExpiresAtMS
			meta[MetaExpired] = time.UnixMilli(token.ExpiresAtMS).UTC().Format(time.RFC3339)
		}
	}
	domain := Identity{Endpoint: endpoint, Domain: tokenDomain}.DomainOrDefault()
	if domain != "" {
		meta[MetaDomain] = domain
	}
	if account != nil {
		meta[MetaUID] = account.UID
		if account.Nickname != "" {
			meta[MetaNickname] = account.Nickname
		}
		if account.EnterpriseID != "" {
			meta[MetaEnterpriseID] = account.EnterpriseID
		}
		if account.DepartmentInfo != "" {
			meta[MetaDepartmentInfo] = account.DepartmentInfo
		}
	}
	return meta
}
