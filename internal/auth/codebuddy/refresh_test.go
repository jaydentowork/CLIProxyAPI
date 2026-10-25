package codebuddy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRefreshTokenSendsRefreshHeaders(t *testing.T) {
	var (
		gotMethod  string
		gotPath    string
		gotHeaders http.Header
		gotBody    string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotHeaders = r.Header.Clone()
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read request body: %v", errRead)
		}
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"data":{"accessToken":"tok-2","expiresIn":1800}}}`))
	}))
	defer server.Close()

	identity := Identity{
		Endpoint:     server.URL,
		AccessToken:  "tok-1",
		RefreshToken: "ref-1",
		MachineID:    "machine-1",
		UID:          "u-1",
	}
	client := server.Client()
	token, err := RefreshToken(context.Background(), client, identity)
	if err != nil {
		t.Fatalf("RefreshToken() error = %v", err)
	}
	if token.AccessToken != "tok-2" {
		t.Errorf("AccessToken = %q, want tok-2", token.AccessToken)
	}
	if token.ExpiresAtMS <= time.Now().UnixMilli() {
		t.Errorf("ExpiresAtMS = %d, want a future expiry derived from expiresIn", token.ExpiresAtMS)
	}
	if token.RefreshToken != "" {
		t.Errorf("RefreshToken = %q, want empty when the response omits it", token.RefreshToken)
	}

	if gotMethod != http.MethodPost || gotPath != RefreshPath {
		t.Errorf("request = %s %s, want POST %s", gotMethod, gotPath, RefreshPath)
	}
	if got := gotHeaders.Get("X-Refresh-Token"); got != "ref-1" {
		t.Errorf("X-Refresh-Token = %q, want ref-1", got)
	}
	if got := gotHeaders.Get("X-Auth-Refresh-Source"); got != "plugin" {
		t.Errorf("X-Auth-Refresh-Source = %q, want plugin", got)
	}
	if got := gotHeaders.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want empty", got)
	}
	if got := gotHeaders.Get("User-Agent"); got != UserAgent {
		t.Errorf("User-Agent = %q, want %q", got, UserAgent)
	}
	if got := gotHeaders.Get("X-Product-Code"); got != ProductCode {
		t.Errorf("X-Product-Code = %q, want %q", got, ProductCode)
	}
	if got := gotHeaders.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if gotBody != "{}" {
		t.Errorf("body = %q, want {}", gotBody)
	}
}

func TestRefreshTokenErrors(t *testing.T) {
	if _, err := RefreshToken(context.Background(), nil, Identity{Endpoint: DefaultEndpoint}); err == nil {
		t.Fatal("RefreshToken() without a refresh token: expected error")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"refresh token expired"}`))
	}))
	defer server.Close()

	_, err := RefreshToken(context.Background(), server.Client(), Identity{Endpoint: server.URL, RefreshToken: "ref-1"})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("RefreshToken() error = %v, want HTTPError 401", err)
	}
}

func TestParseTokenObjectExpiresIn(t *testing.T) {
	now := time.Now()
	token := ParseTokenObject(map[string]any{
		"accessToken":  "tok-1",
		"refreshToken": "ref-1",
		"expiresIn":    float64(1200),
		"domain":       "custom.example",
	}, now)
	if token == nil {
		t.Fatal("ParseTokenObject() = nil, want a token")
	}
	wantExpiry := now.Add(1200 * time.Second).UnixMilli()
	if token.ExpiresAtMS != wantExpiry {
		t.Errorf("ExpiresAtMS = %d, want %d", token.ExpiresAtMS, wantExpiry)
	}
	if token.Domain != "custom.example" || token.RefreshToken != "ref-1" {
		t.Errorf("token = %+v", token)
	}

	if token := ParseTokenObject(map[string]any{"refreshToken": "ref-1"}, now); token != nil {
		t.Errorf("ParseTokenObject() without an access token = %+v, want nil", token)
	}
}

func TestApplyTokenToMetadataKeepsRefreshToken(t *testing.T) {
	now := time.Now()
	meta := map[string]any{
		MetaType:         Provider,
		MetaAccessToken:  "tok-1",
		MetaRefreshToken: "ref-1",
		MetaDomain:       "old.example",
	}

	// A refresh response normally omits the refresh token; the stored one must survive.
	ApplyTokenToMetadata(meta, &TokenResult{AccessToken: "tok-2", ExpiresAtMS: now.Add(time.Hour).UnixMilli()}, now)
	if got := meta[MetaRefreshToken]; got != "ref-1" {
		t.Errorf("%s = %v, want the previous refresh token", MetaRefreshToken, got)
	}
	if got := meta[MetaAccessToken]; got != "tok-2" {
		t.Errorf("%s = %v, want tok-2", MetaAccessToken, got)
	}
	if got := meta[MetaExpiresAt]; got != now.Add(time.Hour).UnixMilli() {
		t.Errorf("%s = %v, want the new expiry", MetaExpiresAt, got)
	}
	if got := meta[MetaExpired]; got != now.Add(time.Hour).UTC().Format(time.RFC3339) {
		t.Errorf("%s = %v, want the RFC3339 expiry", MetaExpired, got)
	}
	if got := meta[MetaDomain]; got != "old.example" {
		t.Errorf("%s = %v, want the previous domain when the response omits it", MetaDomain, got)
	}
	if got := meta[MetaLastRefresh]; got != now.UTC().Format(time.RFC3339) {
		t.Errorf("%s = %v, want the refresh timestamp", MetaLastRefresh, got)
	}

	ApplyTokenToMetadata(meta, &TokenResult{AccessToken: "tok-3", RefreshToken: "ref-2", Domain: "new.example"}, now)
	if got := meta[MetaRefreshToken]; got != "ref-2" {
		t.Errorf("%s = %v, want the rotated refresh token", MetaRefreshToken, got)
	}
	if got := meta[MetaDomain]; got != "new.example" {
		t.Errorf("%s = %v, want the new domain", MetaDomain, got)
	}
}

func TestApplyTokenToMetadataDropsStaleExpiry(t *testing.T) {
	now := time.Now()
	meta := map[string]any{
		MetaType:        Provider,
		MetaAccessToken: "tok-1",
		MetaExpiresAt:   now.Add(-time.Hour).UnixMilli(),
		MetaExpired:     now.Add(-time.Hour).UTC().Format(time.RFC3339),
	}

	// A response without expiresIn must not leave the previous expiry behind, or the
	// credential would keep looking expired and refresh on every request.
	ApplyTokenToMetadata(meta, &TokenResult{AccessToken: "tok-2"}, now)

	if got, ok := meta[MetaExpiresAt]; ok {
		t.Errorf("%s = %v, want the stale expiry removed", MetaExpiresAt, got)
	}
	if got, ok := meta[MetaExpired]; ok {
		t.Errorf("%s = %v, want the stale expiry removed", MetaExpired, got)
	}
	if got := meta[MetaAccessToken]; got != "tok-2" {
		t.Errorf("%s = %v, want tok-2", MetaAccessToken, got)
	}
}
