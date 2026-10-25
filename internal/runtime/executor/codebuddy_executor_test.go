package executor

import (
	"context"
	"encoding/json"
	"fmt"
	codebuddyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func readCodeBuddyTestBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read request body: %v", err)
	}
	return body
}

func codeBuddyTestAuth(endpoint string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "codebuddy-test",
		Provider: codebuddyauth.Provider,
		Label:    "codebuddy-test",
		Metadata: map[string]any{
			codebuddyauth.MetaEndpoint:     endpoint,
			codebuddyauth.MetaAccessToken:  "test-access-token",
			codebuddyauth.MetaRefreshToken: "test-refresh-token",
			codebuddyauth.MetaMachineID:    "machine-1",
			codebuddyauth.MetaDomain:       "www.codebuddy.ai",
			codebuddyauth.MetaUID:          "user-1",
			codebuddyauth.MetaExpiresAt:    time.Now().Add(2 * time.Hour).UnixMilli(),
		},
	}
}

func codeBuddyTestRequest(body string) cliproxyexecutor.Request {
	return cliproxyexecutor.Request{
		Model:   "gpt-5",
		Payload: []byte(body),
	}
}

func codeBuddyTestOptions(stream bool) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		Stream:       stream,
		SourceFormat: sdktranslator.FormatOpenAI,
	}
}

const codeBuddyTestRequestBody = `{"model":"gpt-5","messages":[{"role":"user","content":"first user message"}],"stream":false}`

func codeBuddyTestSSE() string {
	return strings.Join([]string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":""}]}`,
		``,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
}

func TestCodeBuddyExecutorIdentifier(t *testing.T) {
	if got := NewCodeBuddyExecutor(&config.Config{}).Identifier(); got != "codebuddy" {
		t.Fatalf("Identifier() = %q, want codebuddy", got)
	}
}

func TestCodeBuddyExecuteStreamSendsIdentityHeadersAndForcedStream(t *testing.T) {
	var captured *http.Request
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r
		capturedBody = readCodeBuddyTestBody(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codeBuddyTestSSE()))
	}))
	defer server.Close()

	executor := NewCodeBuddyExecutor(&config.Config{})
	auth := codeBuddyTestAuth(server.URL)
	req := codeBuddyTestRequest(codeBuddyTestRequestBody)
	opts := codeBuddyTestOptions(true)
	opts.OriginalRequest = []byte(codeBuddyTestRequestBody)

	result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if result == nil {
		t.Fatal("ExecuteStream() returned nil result")
	}

	var streamed strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		streamed.Write(chunk.Payload)
	}

	if captured == nil {
		t.Fatal("upstream request was not made")
	}
	if got := captured.URL.Path; got != codebuddyauth.ChatPath {
		t.Errorf("upstream path = %q, want %q", got, codebuddyauth.ChatPath)
	}
	if got := captured.Header.Get("Authorization"); got != "Bearer test-access-token" {
		t.Errorf("Authorization = %q, want Bearer test-access-token", got)
	}
	if got := captured.Header.Get("X-Machine-Id"); got != "machine-1" {
		t.Errorf("X-Machine-Id = %q, want machine-1", got)
	}
	if got := captured.Header.Get("X-Product-Code"); got != codebuddyauth.ProductCode {
		t.Errorf("X-Product-Code = %q, want %q", got, codebuddyauth.ProductCode)
	}
	if got := captured.Header.Get("Accept"); got != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", got)
	}
	if got := captured.Header.Get("X-Conversation-ID"); got != codebuddyauth.ConversationID("first user message") {
		t.Errorf("X-Conversation-ID = %q, want %q", got, codebuddyauth.ConversationID("first user message"))
	}
	if got := captured.Header.Get("X-Conversation-Message-ID"); got == "" {
		t.Error("X-Conversation-Message-ID is empty")
	}
	if got := captured.Header.Get("X-Conversation-Request-ID"); got == "" {
		t.Error("X-Conversation-Request-ID is empty")
	}
	if gjson.GetBytes(capturedBody, "stream").Bool() != true {
		t.Errorf("upstream body stream = %v, want true", gjson.GetBytes(capturedBody, "stream"))
	}
	if gjson.GetBytes(capturedBody, "stream_options.include_usage").Bool() != true {
		t.Errorf("upstream stream_options.include_usage = %v, want true", gjson.GetBytes(capturedBody, "stream_options.include_usage"))
	}

	if streamed.Len() == 0 {
		t.Fatal("no stream payload relayed to the client")
	}
	if strings.Contains(streamed.String(), "finish_reason") && strings.Contains(streamed.String(), `"finish_reason":""`) {
		t.Fatalf("empty finish_reason was relayed: %s", streamed.String())
	}
	if !strings.Contains(streamed.String(), "Hello") || !strings.Contains(streamed.String(), " world") {
		t.Fatalf("stream payload lost content: %s", streamed.String())
	}
}

func TestCodeBuddyExecuteStreamNormalizesToolChoice(t *testing.T) {
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody = readCodeBuddyTestBody(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	executor := NewCodeBuddyExecutor(&config.Config{})
	auth := codeBuddyTestAuth(server.URL)
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"function","function":{"name":"lookup"}}}`

	result, err := executor.ExecuteStream(context.Background(), auth, codeBuddyTestRequest(body), codeBuddyTestOptions(true))
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for range result.Chunks {
	}

	choice := gjson.GetBytes(capturedBody, "tool_choice")
	if choice.Type != gjson.String || choice.String() != "lookup" {
		t.Fatalf("tool_choice = %s, want the string \"lookup\"", choice.Raw)
	}
}

func TestCodeBuddyExecuteStreamAppliesPayloadConfigAfterBuiltInTransforms(t *testing.T) {
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody = readCodeBuddyTestBody(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	cfg := &config.Config{
		Payload: config.PayloadConfig{
			Override: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: "gpt-5"}},
				Params: map[string]any{
					"custom_enabled": true,
					"stream":         false,
				},
			}},
			Filter: []config.PayloadFilterRule{{
				Models: []config.PayloadModelRule{{Name: "gpt-5"}},
				Params: []string{"messages.0"},
			}},
		},
	}

	executor := NewCodeBuddyExecutor(cfg)
	auth := codeBuddyTestAuth(server.URL)

	result, err := executor.ExecuteStream(context.Background(), auth, codeBuddyTestRequest(codeBuddyTestRequestBody), codeBuddyTestOptions(true))
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for range result.Chunks {
	}

	if len(capturedBody) == 0 {
		t.Fatal("upstream body is empty")
	}
	// Payload config is the final semantic step: nothing after it may restore the
	// built-in stream:true that the executor forced earlier.
	if gjson.GetBytes(capturedBody, "stream").Bool() != false {
		t.Fatalf("payload override for stream did not survive: %s", capturedBody)
	}
	if gjson.GetBytes(capturedBody, "custom_enabled").Bool() != true {
		t.Fatalf("payload override field is missing: %s", capturedBody)
	}
	if gjson.GetBytes(capturedBody, "messages.0").Exists() {
		t.Fatalf("payload filter did not survive: %s", capturedBody)
	}
}

func TestCodeBuddyExecuteAggregatesStreamForNonStreamClient(t *testing.T) {
	requestedStream := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readCodeBuddyTestBody(t, r)
		requestedStream = gjson.GetBytes(body, "stream").Bool()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codeBuddyTestSSE()))
	}))
	defer server.Close()

	executor := NewCodeBuddyExecutor(&config.Config{})
	auth := codeBuddyTestAuth(server.URL)

	resp, err := executor.Execute(context.Background(), auth, codeBuddyTestRequest(codeBuddyTestRequestBody), codeBuddyTestOptions(false))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !requestedStream {
		t.Fatal("upstream was not called with stream:true")
	}

	payload := resp.Payload
	if !json.Valid(payload) {
		t.Fatalf("response payload is not JSON: %s", payload)
	}
	if got := gjson.GetBytes(payload, "choices.0.message.content").String(); got != "Hello world" {
		t.Fatalf("aggregated content = %q, want %q (payload %s)", got, "Hello world", payload)
	}
	if got := gjson.GetBytes(payload, "usage.total_tokens").Int(); got != 7 {
		t.Errorf("usage.total_tokens = %d, want 7 (payload %s)", got, payload)
	}
}

func TestCodeBuddyExecuteSurfacesRetryAfterFromRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"too many requests"}}`))
	}))
	defer server.Close()

	executor := NewCodeBuddyExecutor(&config.Config{})
	auth := codeBuddyTestAuth(server.URL)

	_, err := executor.Execute(context.Background(), auth, codeBuddyTestRequest(codeBuddyTestRequestBody), codeBuddyTestOptions(false))
	if err == nil {
		t.Fatal("Execute() succeeded on a 429 response")
	}

	statusErr, ok := err.(statusErr)
	if !ok {
		t.Fatalf("error type = %T, want statusErr", err)
	}
	if statusErr.StatusCode() != http.StatusTooManyRequests {
		t.Errorf("StatusCode() = %d, want 429", statusErr.StatusCode())
	}
	retryAfter := statusErr.RetryAfter()
	if retryAfter == nil || *retryAfter != 42*time.Second {
		t.Fatalf("RetryAfter() = %v, want 42s", retryAfter)
	}
}

func TestCodeBuddyExecuteStreamSurfacesRetryAfterFromRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":11128,"message":"rate limited"}`))
	}))
	defer server.Close()

	executor := NewCodeBuddyExecutor(&config.Config{})
	auth := codeBuddyTestAuth(server.URL)

	_, err := executor.ExecuteStream(context.Background(), auth, codeBuddyTestRequest(codeBuddyTestRequestBody), codeBuddyTestOptions(true))
	if err == nil {
		t.Fatal("ExecuteStream() succeeded on a 429 response")
	}
	statusErr, ok := err.(statusErr)
	if !ok {
		t.Fatalf("error type = %T, want statusErr", err)
	}
	if statusErr.StatusCode() != http.StatusTooManyRequests {
		t.Errorf("StatusCode() = %d, want 429", statusErr.StatusCode())
	}
	if retryAfter := statusErr.RetryAfter(); retryAfter == nil || *retryAfter != 7*time.Second {
		t.Fatalf("RetryAfter() = %v, want 7s", retryAfter)
	}
}

func TestCodeBuddyRefreshUpdatesMetadata(t *testing.T) {
	var gotRefreshToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != codebuddyauth.RefreshPath {
			t.Errorf("refresh path = %q, want %q", r.URL.Path, codebuddyauth.RefreshPath)
		}
		gotRefreshToken = r.Header.Get("X-Refresh-Token")
		if r.Header.Get("Authorization") != "" {
			t.Errorf("refresh request must omit Authorization, got %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"accessToken":"fresh-token","refreshToken":"fresh-refresh","expiresAt":` +
			fmt.Sprintf("%d", time.Now().Add(time.Hour).UnixMilli()) + `,"domain":"www.codebuddy.ai"}}`))
	}))
	defer server.Close()

	executor := NewCodeBuddyExecutor(&config.Config{})
	auth := codeBuddyTestAuth(server.URL)

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if gotRefreshToken != "test-refresh-token" {
		t.Errorf("X-Refresh-Token = %q, want test-refresh-token", gotRefreshToken)
	}
	if updated == nil {
		t.Fatal("Refresh() returned nil auth")
	}
	if got := updated.Metadata[codebuddyauth.MetaAccessToken]; got != "fresh-token" {
		t.Errorf("access_token = %v, want fresh-token", got)
	}
	if got := updated.Metadata[codebuddyauth.MetaRefreshToken]; got != "fresh-refresh" {
		t.Errorf("refresh_token = %v, want fresh-refresh", got)
	}
	if got, ok := updated.Metadata[codebuddyauth.MetaExpiresAt]; !ok || got == nil {
		t.Errorf("expires_at was not updated: %v", updated.Metadata)
	}
	if _, ok := updated.Metadata[codebuddyauth.MetaLastRefresh]; !ok {
		t.Error("last_refresh was not updated")
	}
}

func TestCodeBuddyRefreshWithoutRefreshTokenIsNoop(t *testing.T) {
	executor := NewCodeBuddyExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "codebuddy-api-key",
		Provider: codebuddyauth.Provider,
		Metadata: map[string]any{codebuddyauth.MetaAccessToken: "static"},
	}

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if updated != auth {
		t.Fatal("Refresh() without a refresh token must return the same auth record")
	}
}

func TestCodeBuddyCountTokensReturnsUsagePayload(t *testing.T) {
	executor := NewCodeBuddyExecutor(&config.Config{})
	opts := codeBuddyTestOptions(false)
	opts.SourceFormat = sdktranslator.FormatOpenAI

	resp, err := executor.CountTokens(context.Background(), nil, codeBuddyTestRequest(codeBuddyTestRequestBody), opts)
	if err != nil {
		t.Fatalf("CountTokens() error = %v", err)
	}
	if !json.Valid(resp.Payload) {
		t.Fatalf("CountTokens() payload is not JSON: %s", resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "usage.prompt_tokens").Int(); got <= 0 {
		t.Fatalf("usage.prompt_tokens = %d, want > 0 (payload %s)", got, resp.Payload)
	}
}

func TestCodeBuddyExecuteStreamRecoversInlineToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"id":"chatcmpl-7","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":"listing <tool_ca"},"finish_reason":null}]}`,
			``,
			`data: {"id":"chatcmpl-7","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"content":"lls><invoke name=\"bash\"><parameter name=\"cmd\">ls</parameter></invoke></tool_calls>"},"finish_reason":"stop"}]}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n")))
	}))
	defer server.Close()

	executor := NewCodeBuddyExecutor(&config.Config{})
	auth := codeBuddyTestAuth(server.URL)

	result, err := executor.ExecuteStream(context.Background(), auth, codeBuddyTestRequest(codeBuddyTestRequestBody), codeBuddyTestOptions(true))
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var streamed strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		streamed.Write(chunk.Payload)
	}

	payload := streamed.String()
	if strings.Contains(payload, "<tool_ca") || strings.Contains(payload, "<invoke") {
		t.Fatalf("inline markup leaked to the client: %s", payload)
	}
	if !strings.Contains(payload, "listing ") {
		t.Fatalf("visible text was lost: %s", payload)
	}

	// Stream chunks are concatenated by the translator; the recovered call shows up
	// as a native tool call delta with a tool_calls finish reason.
	if !strings.Contains(payload, `"name":"bash"`) {
		t.Fatalf("recovered tool call is missing from the relayed stream: %s", payload)
	}
	if strings.Contains(payload, `"finish_reason":"stop"`) {
		t.Fatalf("recovered tool call stream still reports a normal stop: %s", payload)
	}
}

// TestCodeBuddyExecuteStreamKeepsEveryRecoveredToolCallForClaudeClients relays a
// stream with two inline calls through the real Claude translator to prove that
// neither recovered call is dropped and that the stream reports one tool stop.
func TestCodeBuddyExecuteStreamKeepsEveryRecoveredToolCallForClaudeClients(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"id":"chatcmpl-8","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":"<invoke name=\"bash\"><parameter name=\"cmd\">pwd</parameter></invoke>"},"finish_reason":null}]}`,
			``,
			`data: {"id":"chatcmpl-8","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"content":"<invoke name=\"grep\"><parameter name=\"pattern\">todo</parameter></invoke>"},"finish_reason":null}]}`,
			``,
			`data: {"id":"chatcmpl-8","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n")))
	}))
	defer server.Close()

	executor := NewCodeBuddyExecutor(&config.Config{})
	auth := codeBuddyTestAuth(server.URL)
	opts := codeBuddyTestOptions(true)
	opts.SourceFormat = sdktranslator.FormatClaude

	payload := `{"model":"gpt-5","max_tokens":128,"messages":[{"role":"user","content":"run both"}],"stream":true}`
	result, err := executor.ExecuteStream(context.Background(), auth, codeBuddyTestRequest(payload), opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var streamed strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		streamed.Write(chunk.Payload)
	}

	relayed := streamed.String()
	for _, name := range []string{"bash", "grep"} {
		if !strings.Contains(relayed, `"name":"`+name+`"`) {
			t.Errorf("recovered tool %s is missing from the relayed stream: %s", name, relayed)
		}
	}
	if got := strings.Count(relayed, `"type":"tool_use"`); got != 2 {
		t.Fatalf("tool_use blocks = %d, want 2: %s", got, relayed)
	}
	if got := strings.Count(relayed, `"stop_reason":"tool_use"`); got != 1 {
		t.Fatalf("stop_reason tool_use count = %d, want 1: %s", got, relayed)
	}
}
func TestCodeBuddyRefreshRebuildsStorageFromNewTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"data":{"accessToken":"tok-2","refreshToken":"ref-2","expiresIn":1800,"domain":"www.codebuddy.ai"}}}`))
	}))
	defer server.Close()

	auth := codeBuddyTestAuth(server.URL)
	auth.Storage = &codebuddyauth.CodeBuddyTokenStorage{
		Type:        codebuddyauth.Provider,
		Endpoint:    server.URL,
		AccessToken: "tok-1",
		ExpiresAt:   time.Now().Add(-time.Hour).UnixMilli(),
		Expired:     time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		Metadata:    map[string]any{"injected": "1"},
	}

	refreshed, err := NewCodeBuddyExecutor(&config.Config{}).Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	// A stale storage would make the next save write the expired tokens to disk.
	storage, isStorage := refreshed.Storage.(*codebuddyauth.CodeBuddyTokenStorage)
	if !isStorage {
		t.Fatalf("Storage = %T, want *CodeBuddyTokenStorage", refreshed.Storage)
	}
	if storage.AccessToken != "tok-2" || storage.RefreshToken != "ref-2" {
		t.Errorf("storage tokens = %q/%q, want tok-2/ref-2", storage.AccessToken, storage.RefreshToken)
	}
	if storage.ExpiresAt <= time.Now().UnixMilli() {
		t.Errorf("storage ExpiresAt = %d, want a future expiry", storage.ExpiresAt)
	}
	if storage.UID != "user-1" || storage.MachineID != "machine-1" {
		t.Errorf("storage identity = %q/%q, want user-1/machine-1", storage.UID, storage.MachineID)
	}
	if storage.Metadata["injected"] != "1" {
		t.Errorf("injected metadata was dropped: %v", storage.Metadata)
	}
	if got := refreshed.Metadata[codebuddyauth.MetaAccessToken]; got != "tok-2" {
		t.Errorf("metadata access token = %v, want tok-2", got)
	}
}

func TestCodeBuddyRefreshKeepsStoredRefreshTokenWhenOmitted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// CodeBuddy usually rotates only the access token.
		_, _ = w.Write([]byte(`{"data":{"accessToken":"tok-2","expiresIn":1800}}`))
	}))
	defer server.Close()

	auth := codeBuddyTestAuth(server.URL)
	oldRefresh, _ := auth.Metadata[codebuddyauth.MetaRefreshToken].(string)
	if oldRefresh == "" {
		t.Fatal("test auth has no refresh token")
	}
	auth.Storage = &codebuddyauth.CodeBuddyTokenStorage{
		Type:         codebuddyauth.Provider,
		Endpoint:     server.URL,
		AccessToken:  "tok-1",
		RefreshToken: oldRefresh,
	}

	refreshed, err := NewCodeBuddyExecutor(&config.Config{}).Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	storage, isStorage := refreshed.Storage.(*codebuddyauth.CodeBuddyTokenStorage)
	if !isStorage {
		t.Fatalf("Storage = %T, want *CodeBuddyTokenStorage", refreshed.Storage)
	}
	// Writing an empty refresh token to disk would force a full re-login.
	if storage.AccessToken != "tok-2" || storage.RefreshToken != oldRefresh {
		t.Errorf("storage tokens = %q/%q, want tok-2/%s", storage.AccessToken, storage.RefreshToken, oldRefresh)
	}
	if got := refreshed.Metadata[codebuddyauth.MetaRefreshToken]; got != oldRefresh {
		t.Errorf("metadata refresh token = %v, want %s", got, oldRefresh)
	}
	// The caller's record must stay untouched until the conductor installs the clone.
	if got := auth.Metadata[codebuddyauth.MetaAccessToken]; got == "tok-2" {
		t.Error("Refresh mutated the caller's auth metadata in place")
	}
}

func TestCodeBuddyRefreshMapsUpstreamStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"msg":"token expired"}`))
	}))
	defer server.Close()

	_, err := NewCodeBuddyExecutor(&config.Config{}).Refresh(context.Background(), codeBuddyTestAuth(server.URL))
	if err == nil {
		t.Fatal("Refresh() with a rejected refresh token: expected an error")
	}
	status, isStatus := err.(statusErr)
	if !isStatus {
		t.Fatalf("Refresh() error = %T (%v), want statusErr", err, err)
	}
	if status.code != http.StatusUnauthorized {
		t.Errorf("statusErr.code = %d, want %d", status.code, http.StatusUnauthorized)
	}
}

func TestCodeBuddyShouldPrepareRequestAuth(t *testing.T) {
	executor := NewCodeBuddyExecutor(&config.Config{})

	expiring := codeBuddyTestAuth("https://codebuddy.example")
	expiring.Metadata[codebuddyauth.MetaExpiresAt] = time.Now().Add(5 * time.Second).UnixMilli()
	if !executor.ShouldPrepareRequestAuth(expiring) {
		t.Error("an access token expiring inside the refresh skew must be prepared")
	}

	fresh := codeBuddyTestAuth("https://codebuddy.example")
	fresh.Metadata[codebuddyauth.MetaExpiresAt] = time.Now().Add(2 * time.Hour).UnixMilli()
	if executor.ShouldPrepareRequestAuth(fresh) {
		t.Error("a fresh access token must not be prepared")
	}

	noRefresh := codeBuddyTestAuth("https://codebuddy.example")
	delete(noRefresh.Metadata, codebuddyauth.MetaRefreshToken)
	noRefresh.Metadata[codebuddyauth.MetaExpiresAt] = time.Now().Add(-time.Hour).UnixMilli()
	if executor.ShouldPrepareRequestAuth(noRefresh) {
		t.Error("a credential without a refresh token must not be prepared")
	}

	if executor.ShouldPrepareRequestAuth(nil) {
		t.Error("nil auth must not be prepared")
	}
}

func TestCodeBuddyExecuteAppliesPayloadConfigAfterBuiltInTransforms(t *testing.T) {
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody = readCodeBuddyTestBody(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codeBuddyTestSSE()))
	}))
	defer server.Close()

	executor := NewCodeBuddyExecutor(codeBuddyTestPayloadConfig())

	resp, errExec := executor.Execute(context.Background(), codeBuddyTestAuth(server.URL), codeBuddyTestRequest(codeBuddyTestRequestBody), codeBuddyTestOptions(false))
	if errExec != nil {
		t.Fatalf("Execute() error = %v", errExec)
	}
	if len(resp.Payload) == 0 {
		t.Fatal("Execute() returned an empty payload")
	}

	// Payload config is the final semantic step: nothing after it may restore the
	// built-in stream:true that the executor forced earlier.
	if gjson.GetBytes(capturedBody, "stream").Bool() {
		t.Fatalf("payload override for stream did not survive: %s", capturedBody)
	}
	if !gjson.GetBytes(capturedBody, "custom_enabled").Bool() {
		t.Fatalf("payload override field is missing: %s", capturedBody)
	}
	if gjson.GetBytes(capturedBody, "messages.0").Exists() {
		t.Fatalf("payload filter did not survive: %s", capturedBody)
	}
}

func TestCodeBuddyCountTokensAppliesPayloadConfigAfterBuiltInTransforms(t *testing.T) {
	auth := codeBuddyTestAuth("https://codebuddy.example")
	long := `{"model":"gpt-5","messages":[{"role":"user","content":"` + strings.Repeat("token ", 64) + `"},{"role":"user","content":"second"}],"stream":false}`

	withConfig, errConfigured := NewCodeBuddyExecutor(codeBuddyTestPayloadConfig()).CountTokens(context.Background(), auth, codeBuddyTestRequest(long), codeBuddyTestOptions(false))
	if errConfigured != nil {
		t.Fatalf("CountTokens() with payload config error = %v", errConfigured)
	}
	withoutConfig, errPlain := NewCodeBuddyExecutor(&config.Config{}).CountTokens(context.Background(), auth, codeBuddyTestRequest(long), codeBuddyTestOptions(false))
	if errPlain != nil {
		t.Fatalf("CountTokens() without payload config error = %v", errPlain)
	}

	// The filter runs on the counted payload, so the filtered message cannot be part
	// of the reported prompt tokens.
	filtered := gjson.GetBytes(withConfig.Payload, "usage.prompt_tokens").Int()
	unfiltered := gjson.GetBytes(withoutConfig.Payload, "usage.prompt_tokens").Int()
	if filtered <= 0 || filtered >= unfiltered {
		t.Fatalf("filtered prompt_tokens = %d, want fewer than the unfiltered %d", filtered, unfiltered)
	}
}

// codeBuddyTestPayloadConfig overrides stream back to false and filters the first
// message, so a payload-config-last regression is observable in the upstream body.
func codeBuddyTestPayloadConfig() *config.Config {
	return &config.Config{
		Payload: config.PayloadConfig{
			Override: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: "gpt-5"}},
				Params: map[string]any{
					"custom_enabled": true,
					"stream":         false,
				},
			}},
			Filter: []config.PayloadFilterRule{{
				Models: []config.PayloadModelRule{{Name: "gpt-5"}},
				Params: []string{"messages.0"},
			}},
		},
	}
}
