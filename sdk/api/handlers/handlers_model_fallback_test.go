package handlers

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type modelFallbackTestExecutor struct {
	mu        sync.Mutex
	models    []string
	execute   func(model string) (coreexecutor.Response, error)
	stream    func(model string) []coreexecutor.StreamChunk
	streamErr func(model string) error
	exited    chan string
}

func (e *modelFallbackTestExecutor) Identifier() string { return "fallback-test" }

func (e *modelFallbackTestExecutor) record(model string) {
	e.mu.Lock()
	e.models = append(e.models, model)
	e.mu.Unlock()
}

func (e *modelFallbackTestExecutor) Models() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.models...)
}

func (e *modelFallbackTestExecutor) Execute(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.record(req.Model)
	return e.execute(req.Model)
}

func (e *modelFallbackTestExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.record(req.Model)
	if e.streamErr != nil {
		if errStream := e.streamErr(req.Model); errStream != nil {
			return nil, errStream
		}
	}
	chunks := e.stream(req.Model)
	ch := make(chan coreexecutor.StreamChunk)
	go func() {
		defer close(ch)
		if e.exited != nil {
			defer func() { e.exited <- req.Model }()
		}
		for _, chunk := range chunks {
			select {
			case ch <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func (e *modelFallbackTestExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *modelFallbackTestExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *modelFallbackTestExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

type modelFallbackTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *modelFallbackTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *modelFallbackTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newModelFallbackTestHandler(t *testing.T, executor *modelFallbackTestExecutor, rule sdkconfig.ModelFallbackRule) (*BaseAPIHandler, *modelFallbackTestClock) {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "fallback-auth-" + t.Name(), Provider: "fallback-test", Status: coreauth.StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("manager.Register(): %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "model-a"}, {ID: "model-b"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{Routing: sdkconfig.RoutingConfig{ModelFallbacks: []sdkconfig.ModelFallbackRule{rule}}}, manager)
	clock := &modelFallbackTestClock{now: time.Unix(1_700_000_000, 0)}
	handler.modelFallback = newModelFallbackState(clock.Now)
	return handler, clock
}

func modelFallbackSessionRequest(model, session string) []byte {
	return []byte(`{"model":"` + model + `","metadata":{"user_id":"session_` + session + `"},"messages":[{"role":"user","content":"hello"}]}`)
}

func TestModelFallbackNonStream(t *testing.T) {
	cases := []struct {
		name    string
		refuse  func() (coreexecutor.Response, error)
		handler string
	}{
		{
			name: "claude policy refusal error",
			refuse: func() (coreexecutor.Response, error) {
				return coreexecutor.Response{}, &coreauth.Error{Code: "invalid_request", Message: "claude refusal (cyber): blocked", HTTPStatus: http.StatusBadRequest}
			},
			handler: "claude",
		},
		{
			name: "http 200 stop_reason refusal",
			refuse: func() (coreexecutor.Response, error) {
				return coreexecutor.Response{Payload: []byte(`{"type":"message","stop_reason":"refusal","content":[]}`)}, nil
			},
			handler: "claude",
		},
		{
			name: "finish_reason content_filter",
			refuse: func() (coreexecutor.Response, error) {
				return coreexecutor.Response{Payload: []byte(`{"choices":[{"index":0,"finish_reason":"content_filter","message":{"role":"assistant","content":""}}]}`)}, nil
			},
			handler: "openai",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executor := &modelFallbackTestExecutor{execute: func(model string) (coreexecutor.Response, error) {
				if model == "model-a" {
					return tc.refuse()
				}
				return coreexecutor.Response{Payload: []byte(`{"ok":"` + model + `"}`)}, nil
			}}
			handler, _ := newModelFallbackTestHandler(t, executor, sdkconfig.ModelFallbackRule{From: "MODEL-A", To: []string{"model-b"}})
			body, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), tc.handler, "model-a", modelFallbackSessionRequest("model-a", "s1"), "")
			if errMsg != nil {
				t.Fatalf("first turn error = %v", errMsg.Error)
			}
			if !strings.Contains(string(body), "model-b") {
				t.Fatalf("first turn body = %s, want fallback model-b", body)
			}
			_, _, errMsg = handler.ExecuteWithAuthManager(context.Background(), tc.handler, "model-a", modelFallbackSessionRequest("model-a", "s1"), "")
			if errMsg != nil {
				t.Fatalf("second turn error = %v", errMsg.Error)
			}
			if got, want := strings.Join(executor.Models(), ","), "model-a,model-b,model-b"; got != want {
				t.Fatalf("executed models = %s, want %s", got, want)
			}
		})
	}
}

func TestModelFallbackNonStreamLastCandidateReturnsRefusal(t *testing.T) {
	executor := &modelFallbackTestExecutor{execute: func(string) (coreexecutor.Response, error) {
		return coreexecutor.Response{Payload: []byte(`{"stop_reason":"refusal"}`)}, nil
	}}
	handler, _ := newModelFallbackTestHandler(t, executor, sdkconfig.ModelFallbackRule{From: "model-*", Fallback: "model-b"})
	body, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "claude", "model-a", modelFallbackSessionRequest("model-a", "s2"), "")
	if errMsg != nil || !strings.Contains(string(body), "refusal") {
		t.Fatalf("body = %s err = %v, want the final refusal unchanged", body, errMsg)
	}
	if got, want := strings.Join(executor.Models(), ","), "model-a,model-b"; got != want {
		t.Fatalf("executed models = %s, want %s", got, want)
	}
}

func modelFallbackClaudeRefusalStream(withErr bool) []coreexecutor.StreamChunk {
	chunks := []coreexecutor.StreamChunk{
		{Payload: []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"refused\",\"role\":\"assistant\"}}\n\n")},
		{Payload: []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")},
		{Payload: []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\",\"stop_details\":{\"type\":\"refusal\",\"category\":\"cyber\"}}}\n\n")},
		{Payload: []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")},
	}
	if withErr {
		chunks = append(chunks, coreexecutor.StreamChunk{Err: &coreauth.Error{Code: "invalid_request", Message: "claude refusal (cyber)", HTTPStatus: http.StatusBadRequest}})
	}
	return chunks
}

func modelFallbackClaudeSuccessStream() []coreexecutor.StreamChunk {
	return []coreexecutor.StreamChunk{
		{Payload: []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"fallback\",\"role\":\"assistant\"}}\n\n")},
		{Payload: []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")},
		{Payload: []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")},
	}
}

func collectModelFallbackStream(t *testing.T, dataChan <-chan []byte, errChan <-chan *interfaces.ErrorMessage) string {
	t.Helper()
	var out strings.Builder
	for chunk := range dataChan {
		out.Write(chunk)
	}
	for errMsg := range errChan {
		if errMsg != nil {
			t.Fatalf("stream error = %v", errMsg.Error)
		}
	}
	return out.String()
}

func TestModelFallbackStreamClaudeRefusal(t *testing.T) {
	for _, withErr := range []bool{false, true} {
		name := "without error chunk"
		if withErr {
			name = "with policy refusal error chunk"
		}
		t.Run(name, func(t *testing.T) {
			executor := &modelFallbackTestExecutor{
				exited: make(chan string, 8),
				stream: func(model string) []coreexecutor.StreamChunk {
					if model == "model-a" {
						return modelFallbackClaudeRefusalStream(withErr)
					}
					return modelFallbackClaudeSuccessStream()
				},
			}
			handler, _ := newModelFallbackTestHandler(t, executor, sdkconfig.ModelFallbackRule{From: "model-a", To: []string{"model-b"}})
			dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "claude", "model-a", modelFallbackSessionRequest("model-a", "s3"), "")
			out := collectModelFallbackStream(t, dataChan, errChan)
			if strings.Contains(out, `"refused"`) || strings.Contains(out, "refusal") {
				t.Fatalf("stream leaked refused attempt: %s", out)
			}
			if !strings.Contains(out, `"fallback"`) || !strings.Contains(out, "text_delta") || !strings.Contains(out, "message_stop") {
				t.Fatalf("stream = %s, want the full fallback stream", out)
			}
			select {
			case got := <-executor.exited:
				if got != "model-a" && got != "model-b" {
					t.Fatalf("exited producer = %s", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("abandoned producer did not exit")
			}
			dataChan, _, errChan = handler.ExecuteStreamWithAuthManager(context.Background(), "claude", "model-a", modelFallbackSessionRequest("model-a", "s3"), "")
			_ = collectModelFallbackStream(t, dataChan, errChan)
			if got, want := strings.Join(executor.Models(), ","), "model-a,model-b,model-b"; got != want {
				t.Fatalf("executed models = %s, want %s", got, want)
			}
		})
	}
}

func TestModelFallbackStreamCommitsSubstantiveOutput(t *testing.T) {
	executor := &modelFallbackTestExecutor{stream: func(string) []coreexecutor.StreamChunk {
		return modelFallbackClaudeSuccessStream()
	}}
	handler, _ := newModelFallbackTestHandler(t, executor, sdkconfig.ModelFallbackRule{From: "model-a", To: []string{"model-b"}})
	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "claude", "model-a", modelFallbackSessionRequest("model-a", "s4"), "")
	out := collectModelFallbackStream(t, dataChan, errChan)
	if strings.Index(out, "message_start") > strings.Index(out, "text_delta") || !strings.Contains(out, "message_stop") {
		t.Fatalf("stream = %s, want buffered preamble flushed in order", out)
	}
	if got := strings.Join(executor.Models(), ","); got != "model-a" {
		t.Fatalf("executed models = %s, want model-a only", got)
	}
}

func TestModelFallbackHeaderlessStickinessAndTTL(t *testing.T) {
	executor := &modelFallbackTestExecutor{execute: func(model string) (coreexecutor.Response, error) {
		if model == "model-a" {
			return coreexecutor.Response{Payload: []byte(`{"stop_reason":"refusal"}`)}, nil
		}
		return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
	}}
	handler, clock := newModelFallbackTestHandler(t, executor, sdkconfig.ModelFallbackRule{From: "model-a", To: []string{"model-b"}, SessionTTL: "10m"})
	turn1 := []byte(`{"model":"model-a","messages":[{"role":"user","content":"first question"}]}`)
	turn2 := []byte(`{"model":"model-a","messages":[{"role":"user","content":"first question"},{"role":"assistant","content":"answer"},{"role":"user","content":"follow up"}]}`)
	execOptions := modelExecutionOptions{CallerScope: "tenant-a"}
	run := func(raw []byte) {
		t.Helper()
		if _, _, errMsg := handler.executeWithAuthManagerFormats(context.Background(), "openai", "openai", "model-a", raw, "", false, execOptions); errMsg != nil {
			t.Fatalf("execute error = %v", errMsg.Error)
		}
	}
	run(turn1)
	clock.Advance(5 * time.Minute)
	run(turn2)
	if got, want := strings.Join(executor.Models(), ","), "model-a,model-b,model-b"; got != want {
		t.Fatalf("executed models = %s, want %s (multi-turn pin)", got, want)
	}
	clock.Advance(11 * time.Minute)
	run(turn2)
	if got, want := strings.Join(executor.Models(), ","), "model-a,model-b,model-b,model-a,model-b"; got != want {
		t.Fatalf("executed models = %s, want %s (pin expired)", got, want)
	}
}

func TestModelFallbackPatternMatching(t *testing.T) {
	cases := []struct {
		pattern, model string
		want           bool
	}{
		{"claude-opus-4.5", "Claude-Opus-4-5", true},
		{"claude-opus-4-5", "claude-opus-4-5(high)", true},
		{"claude-*", "claude-sonnet-4", true},
		{"gpt-*-mini", "gpt-5-mini", true},
		{"gpt-*-mini", "gpt-5", false},
		{"", "gpt-5", false},
	}
	for _, tc := range cases {
		if got := matchModelFallbackPattern(tc.pattern, tc.model); got != tc.want {
			t.Errorf("matchModelFallbackPattern(%q, %q) = %v, want %v", tc.pattern, tc.model, got, tc.want)
		}
	}
}

func TestModelFallbackTriggerDetection(t *testing.T) {
	rule := sdkconfig.ModelFallbackRule{}
	triggers := []string{
		`{"stop_reason":"sensitive"}`,
		`{"incomplete_details":{"reason":"content_filter"}}`,
		`{"candidates":[{"finishReason":"PROHIBITED_CONTENT"}]}`,
		`{"promptFeedback":{"blockReason":"SAFETY"}}`,
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"content_filter\"}]}\n\n",
	}
	for _, payload := range triggers {
		if !isModelFallbackTriggerPayload([]byte(payload), "openai", rule) {
			t.Errorf("payload %s did not trigger fallback", payload)
		}
	}
	if isModelFallbackTriggerPayload([]byte(`{"stop_reason":"end_turn"}`), "claude", rule) {
		t.Error("end_turn triggered fallback")
	}
	if isModelFallbackTriggerPayload([]byte(`{"stop_reason":"refusal"}`), "claude", sdkconfig.ModelFallbackRule{On: []string{"content-filter"}}) {
		t.Error("refusal triggered a content-filter-only rule")
	}
	if !isModelFallbackTriggerError(&coreauth.Error{Message: "Output blocked by content filtering policy"}, rule) {
		t.Error("content filtering error did not trigger fallback")
	}
	if rewritten := string(rewriteModelFallbackBody([]byte(`{"model":"a","x":1}`), "b")); !strings.Contains(rewritten, `"model":"b"`) {
		t.Errorf("rewritten body = %s", rewritten)
	}
}

func TestModelFallbackNonStreamFailingFallbackIsNotPinned(t *testing.T) {
	executor := &modelFallbackTestExecutor{execute: func(model string) (coreexecutor.Response, error) {
		if model == "model-a" {
			return coreexecutor.Response{Payload: []byte(`{"stop_reason":"refusal"}`)}, nil
		}
		return coreexecutor.Response{}, &coreauth.Error{Code: "unavailable", Message: "upstream unavailable", HTTPStatus: http.StatusBadGateway}
	}}
	handler, _ := newModelFallbackTestHandler(t, executor, sdkconfig.ModelFallbackRule{From: "model-a", To: []string{"model-b"}})
	for range 2 {
		if _, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "claude", "model-a", modelFallbackSessionRequest("model-a", "s5"), ""); errMsg == nil {
			t.Fatal("expected the fallback failure to surface")
		}
	}
	// The fallback's 502 may cool down its auth, so only the restart at model-a is asserted.
	if got := executor.Models(); len(got) < 3 || got[0] != "model-a" || !containsModelFallbackSequence(got, "model-b", "model-a") {
		t.Fatalf("executed models = %v, want the second turn to restart at model-a", got)
	}
}

func containsModelFallbackSequence(models []string, first, second string) bool {
	for index := 0; index+1 < len(models); index++ {
		if models[index] == first && models[index+1] == second {
			return true
		}
	}
	return false
}

func TestModelFallbackStreamFailingFallbackIsNotPinned(t *testing.T) {
	executor := &modelFallbackTestExecutor{
		streamErr: func(model string) error {
			if model == "model-a" {
				return &coreauth.Error{Code: "invalid_request", Message: "claude refusal (cyber)", HTTPStatus: http.StatusBadRequest}
			}
			return nil
		},
		stream: func(string) []coreexecutor.StreamChunk {
			return []coreexecutor.StreamChunk{{Err: &coreauth.Error{Code: "bad_request", Message: "unsupported parameter", HTTPStatus: http.StatusBadRequest}}}
		},
	}
	handler, _ := newModelFallbackTestHandler(t, executor, sdkconfig.ModelFallbackRule{From: "model-a", To: []string{"model-b"}})
	for range 2 {
		dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "claude", "model-a", modelFallbackSessionRequest("model-a", "s6"), "")
		if dataChan != nil {
			for range dataChan {
			}
		}
		gotErr := false
		for errMsg := range errChan {
			gotErr = gotErr || errMsg != nil
		}
		if !gotErr {
			t.Fatal("expected the fallback stream error to surface")
		}
	}
	if got := executor.Models(); !containsModelFallbackSequence(got, "model-b", "model-a") {
		t.Fatalf("executed models = %v, want ExecuteStream refusal fallback and no pin", got)
	}
}

func TestModelFallbackStreamTranslatedProtocols(t *testing.T) {
	cases := []struct {
		handlerType string
		refused     []coreexecutor.StreamChunk
		success     []coreexecutor.StreamChunk
	}{
		{
			handlerType: "openai",
			refused: []coreexecutor.StreamChunk{
				{Payload: []byte("data: {\"id\":\"refused\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n")},
				{Payload: []byte("data: {\"id\":\"refused\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"content_filter\"}]}\n\n")},
			},
			success: []coreexecutor.StreamChunk{
				{Payload: []byte("data: {\"id\":\"fallback\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")},
				{Payload: []byte("data: [DONE]\n\n")},
			},
		},
		{
			handlerType: "openai-response",
			refused: []coreexecutor.StreamChunk{
				{Payload: []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"refused\"}}\n\n")},
				{Payload: []byte("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"content\":[]}}\n\n")},
				{Payload: []byte("event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n")},
				{Payload: []byte("event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"refused\",\"incomplete_details\":{\"reason\":\"content_filter\"}}}\n\n")},
			},
			success: []coreexecutor.StreamChunk{
				{Payload: []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"fallback\"}\n\n")},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.handlerType, func(t *testing.T) {
			executor := &modelFallbackTestExecutor{stream: func(model string) []coreexecutor.StreamChunk {
				if model == "model-a" {
					return tc.refused
				}
				return tc.success
			}}
			handler, _ := newModelFallbackTestHandler(t, executor, sdkconfig.ModelFallbackRule{From: "model-a", To: []string{"model-b"}, On: []string{"refusal"}})
			dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), tc.handlerType, "model-a", modelFallbackSessionRequest("model-a", "s7"), "")
			out := collectModelFallbackStream(t, dataChan, errChan)
			if strings.Contains(out, "refused") || !strings.Contains(out, "fallback") {
				t.Fatalf("stream = %s, want only the fallback stream", out)
			}
		})
	}
}

func TestModelFallbackBrokenPinLapsesAfterTTL(t *testing.T) {
	var mu sync.Mutex
	modelBBroken := false
	executor := &modelFallbackTestExecutor{execute: func(model string) (coreexecutor.Response, error) {
		mu.Lock()
		broken := modelBBroken
		mu.Unlock()
		switch {
		case model == "model-b" && broken:
			return coreexecutor.Response{}, &coreauth.Error{Code: "bad_request", Message: "unsupported parameter", HTTPStatus: http.StatusBadRequest}
		case model == "model-b":
			return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
		}
		return coreexecutor.Response{Payload: []byte(`{"stop_reason":"refusal"}`)}, nil
	}}
	handler, clock := newModelFallbackTestHandler(t, executor, sdkconfig.ModelFallbackRule{From: "model-a", To: []string{"model-b"}, SessionTTL: "10m"})
	run := func() {
		t.Helper()
		_, _, _ = handler.ExecuteWithAuthManager(context.Background(), "claude", "model-a", modelFallbackSessionRequest("model-a", "s8"), "")
	}
	run()
	mu.Lock()
	modelBBroken = true
	mu.Unlock()
	for range 3 {
		clock.Advance(4 * time.Minute)
		run()
	}
	if got, want := strings.Join(executor.Models(), ","), "model-a,model-b,model-b,model-b,model-a,model-b"; got != want {
		t.Fatalf("executed models = %s, want %s (failing pin must lapse after its TTL)", got, want)
	}
}
