package codebuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TokenResult is a normalized CodeBuddy token object returned by the auth
// token poll or the refresh endpoint.
type TokenResult struct {
	AccessToken  string
	RefreshToken string
	// ExpiresAtMS is the absolute expiry in epoch milliseconds (0 when unknown).
	ExpiresAtMS int64
	Domain      string
	// Raw is the unwrapped upstream token object, kept for diagnostics.
	Raw map[string]any
}

// UnwrapEnvelope peels CodeBuddy's {"data":{"data":X}} or {"data":X} envelopes.
func UnwrapEnvelope(payload any) any {
	for i := 0; i < 2; i++ {
		obj, ok := payload.(map[string]any)
		if !ok {
			return payload
		}
		inner, ok := obj["data"]
		if !ok || inner == nil {
			return payload
		}
		payload = inner
	}
	return payload
}

// ParseTokenObject converts an unwrapped token object into a TokenResult.
// It returns nil when the object has no access token.
func ParseTokenObject(obj map[string]any, now time.Time) *TokenResult {
	if obj == nil {
		return nil
	}
	access := metaString(obj, "accessToken")
	if access == "" {
		return nil
	}
	tr := &TokenResult{
		AccessToken:  access,
		RefreshToken: metaString(obj, "refreshToken"),
		ExpiresAtMS:  metaInt64(obj, "expiresAt"),
		Domain:       metaString(obj, "domain"),
		Raw:          obj,
	}
	if tr.ExpiresAtMS <= 0 {
		if secs := metaInt64(obj, "expiresIn"); secs > 0 {
			tr.ExpiresAtMS = now.Add(time.Duration(secs) * time.Second).UnixMilli()
		}
	} else if tr.ExpiresAtMS < 1e11 {
		// Defensive: treat second-precision timestamps as seconds.
		tr.ExpiresAtMS *= 1000
	}
	return tr
}

// ApplyTokenToMetadata merges a token result into auth metadata. A refresh
// response that omits the refresh token keeps the previous one.
func ApplyTokenToMetadata(meta map[string]any, tr *TokenResult, now time.Time) {
	if meta == nil || tr == nil {
		return
	}
	meta[MetaType] = Provider
	meta[MetaAccessToken] = tr.AccessToken
	if tr.RefreshToken != "" {
		meta[MetaRefreshToken] = tr.RefreshToken
	}
	if tr.ExpiresAtMS > 0 {
		meta[MetaExpiresAt] = tr.ExpiresAtMS
		meta[MetaExpired] = time.UnixMilli(tr.ExpiresAtMS).UTC().Format(time.RFC3339)
	} else {
		// The response carried no expiry: drop the stale one so NeedsRefresh cannot
		// keep reporting an expired token and re-refresh forever.
		delete(meta, MetaExpiresAt)
		delete(meta, MetaExpired)
	}
	if tr.Domain != "" {
		meta[MetaDomain] = tr.Domain
	}
	meta[MetaLastRefresh] = now.UTC().Format(time.RFC3339)
}

// RefreshToken exchanges the identity's refresh token for a new token.
// The request carries X-Refresh-Token and deliberately omits Authorization.
func RefreshToken(ctx context.Context, httpClient *http.Client, id Identity) (*TokenResult, error) {
	if strings.TrimSpace(id.RefreshToken) == "" {
		return nil, fmt.Errorf("codebuddy: refresh token missing")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	endpoint := NormalizeEndpoint(id.Endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BuildURL(endpoint, RefreshPath), bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("codebuddy: build refresh request: %w", err)
	}
	for k, v := range id.BaseHeaders(false) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Refresh-Token", id.RefreshToken)
	req.Header.Set("X-Auth-Refresh-Source", "plugin")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: refresh request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("codebuddy: read refresh response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	var payload any
	if err = json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("codebuddy: decode refresh response: %w", err)
	}
	obj, _ := UnwrapEnvelope(payload).(map[string]any)
	tr := ParseTokenObject(obj, time.Now())
	if tr == nil {
		return nil, fmt.Errorf("codebuddy: refresh response missing accessToken")
	}
	return tr, nil
}

// HTTPError is a non-2xx CodeBuddy auth response.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	body := e.Body
	if len(body) > 300 {
		body = body[:300]
	}
	return fmt.Sprintf("codebuddy: upstream status %d: %s", e.StatusCode, body)
}
