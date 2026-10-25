package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	codebuddyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

// CodeBuddyExecutor serves the Tencent CodeBuddy global gateway
// (www.codebuddy.ai) through its OpenAI-compatible chat completions API.
//
// The gateway only speaks SSE: every upstream request carries stream:true, and
// non-streaming clients are served by aggregating the SSE stream into one
// chat.completion payload before the normal non-stream translation runs.
type CodeBuddyExecutor struct {
	cfg *config.Config
}

// NewCodeBuddyExecutor creates a CodeBuddy executor bound to the given config.
func NewCodeBuddyExecutor(cfg *config.Config) *CodeBuddyExecutor {
	return &CodeBuddyExecutor{cfg: cfg}
}

// Identifier returns the provider identifier.
func (e *CodeBuddyExecutor) Identifier() string { return codebuddyauth.Provider }

// RequestToFormat reports the upstream request format used after auth selection.
func (e *CodeBuddyExecutor) RequestToFormat(_ cliproxyexecutor.Request, _ cliproxyexecutor.Options) sdktranslator.Format {
	return sdktranslator.FormatOpenAI
}

// PrepareRequest injects CodeBuddy credentials and identity headers into the
// outgoing HTTP request.
func (e *CodeBuddyExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	identity := codeBuddyIdentity(auth)
	for key, value := range identity.BaseHeaders(true) {
		req.Header.Set(key, value)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects CodeBuddy credentials into the request and executes it.
func (e *CodeBuddyExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, statusErr{code: http.StatusBadRequest, msg: "codebuddy executor: request is nil"}
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// Execute runs a non-streaming chat completion request. The gateway only emits
// SSE, so the upstream call still carries stream:true and the sanitized stream is
// aggregated into one chat.completion payload before the usual non-stream
// translation runs.
func (e *CodeBuddyExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := bytes.Clone(originalPayloadSource)

	upstreamModel := codeBuddyUpstreamModel(baseModel)

	body, err := e.buildRequestBody(ctx, req, opts, baseModel, upstreamModel, to)
	if err != nil {
		return resp, err
	}
	reporter.SetUpstreamModel(upstreamModel)
	reporter.SetTranslatedReasoningEffort(body, to.String())

	httpReq, err := e.newUpstreamChatRequest(ctx, auth, body, req, originalPayload)
	if err != nil {
		return resp, err
	}
	httpResp, err := e.doUpstream(ctx, auth, httpReq, reporter)
	if err != nil {
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codebuddy executor: close response body error: %v", errClose)
		}
	}()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		upstreamBody, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, upstreamBody)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), upstreamBody))
		return resp, newOpenAICompatStatusError(httpResp.StatusCode, httpResp.Header, upstreamBody)
	}

	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)

	aggregated := helps.CodeBuddyStreamAggregatorForSSE(data, helps.NewCodeBuddyStreamProcessor())
	if len(aggregated) == 0 {
		return resp, statusErr{code: http.StatusBadGateway, msg: "codebuddy executor: upstream stream carried no completion"}
	}
	reporter.ObserveResponseModel(aggregated)

	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, helps.ApplyPatchOriginalRequest(req, opts), body, aggregated, &param)
	if len(out) == 0 || helps.ApplyPatchTranslationError(param) != nil {
		return resp, statusErr{code: http.StatusBadGateway, msg: "codebuddy executor: failed to translate upstream response"}
	}
	reporter.Publish(ctx, helps.ParseOpenAIUsage(aggregated))
	if responseFormat == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

// ExecuteStream relays the upstream SSE stream to the client. Every chunk runs
// through a request-scoped CodeBuddy stream processor, which strips inline tool
// call markup, injects recovered tool calls, drops empty finish_reason values,
// backfills tool call names, and holds back partial markup until the stream ends.
func (e *CodeBuddyExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	reporter.SetStream(true)
	defer reporter.TrackFailure(ctx, &err)

	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := bytes.Clone(originalPayloadSource)

	upstreamModel := codeBuddyUpstreamModel(baseModel)

	body, err := e.buildRequestBody(ctx, req, opts, baseModel, upstreamModel, to)
	if err != nil {
		return nil, err
	}
	reporter.SetUpstreamModel(upstreamModel)
	reporter.SetTranslatedReasoningEffort(body, to.String())

	httpReq, err := e.newUpstreamChatRequest(ctx, auth, body, req, originalPayload)
	if err != nil {
		return nil, err
	}
	httpResp, err := e.doUpstream(ctx, auth, httpReq, reporter)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		upstreamBody, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, upstreamBody)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), upstreamBody))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codebuddy executor: close response body error: %v", errClose)
		}
		return nil, newOpenAICompatStatusError(httpResp.StatusCode, httpResp.Header, upstreamBody)
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codebuddy executor: close response body error: %v", errClose)
			}
		}()

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 1_048_576) // 1MB
		claudeInputTokens := helps.NewClaudeInputTokenState(opts.SourceFormat, to, responseFormat, originalPayload)
		var streamUsage helps.StreamUsageBuffer
		defer streamUsage.Publish(ctx, reporter)
		var param any
		helps.InitializeApplyPatchStream(ctx, to, responseFormat, req.Model, helps.ApplyPatchOriginalRequest(req, opts), body, &param)
		processor := helps.NewCodeBuddyStreamProcessor()
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			reporter.ObserveResponseModel(line)
			streamUsage.ObserveOpenAIStream(line)
			payload, ok := helps.CodeBuddyChunkDataLine(line)
			if !ok {
				continue
			}
			sanitized, keep, errSanitize := processor.SanitizeChunk(payload, false)
			if errSanitize != nil {
				if !streamUsage.PublishFailure(ctx, reporter, errSanitize) {
					reporter.PublishFailure(ctx, errSanitize)
				}
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: errSanitize}:
				case <-ctx.Done():
				}
				return
			}
			if !keep {
				continue
			}
			streamLine := append([]byte(helps.CodeBuddySSEDataPrefix), sanitized...)
			chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, helps.ApplyPatchOriginalRequest(req, opts), body, streamLine, &param, claudeInputTokens)
			helps.RecordApplyPatchStreamFailure(ctx, param, reporter, statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage})
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return
				}
			}
			if helps.StopApplyPatchStream(ctx, param, reporter, out, statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage}) {
				return
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			if !streamUsage.PublishFailure(ctx, reporter, errScan) {
				reporter.PublishFailure(ctx, errScan)
			}
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		residual, errResidual := processor.ResidualChunk()
		if errResidual != nil {
			if !streamUsage.PublishFailure(ctx, reporter, errResidual) {
				reporter.PublishFailure(ctx, errResidual)
			}
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errResidual}:
			case <-ctx.Done():
			}
			return
		}
		if len(residual) > 0 {
			residualLine := append([]byte(helps.CodeBuddySSEDataPrefix), residual...)
			residualChunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, helps.ApplyPatchOriginalRequest(req, opts), body, residualLine, &param, claudeInputTokens)
			helps.RecordApplyPatchStreamFailure(ctx, param, reporter, statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage})
			for i := range residualChunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: residualChunks[i]}:
				case <-ctx.Done():
					return
				}
			}
		}
		if helps.EndApplyPatchStream(ctx, param, reporter, out, statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage}) {
			return
		}
		doneChunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, helps.ApplyPatchOriginalRequest(req, opts), body, []byte(helps.CodeBuddySSEDataPrefix+"[DONE]"), &param, claudeInputTokens)
		helps.RecordApplyPatchStreamFailure(ctx, param, reporter, statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage})
		for i := range doneChunks {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: doneChunks[i]}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// CountTokens estimates tokens for a CodeBuddy request. The gateway exposes no
// token counting endpoint, so the shared OpenAI chat tokenizer is used.
func (e *CodeBuddyExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("openai")
	isCompat := helps.APIKeyModelIsCompat(req)

	translated, updatesChanged, err := helps.TranslateRequestWithAPIKeyModelCompatibilityAndUpdateIntent(ctx, opts.Headers, e.cfg, from, to, baseModel, req.Payload, false, isCompat)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	originalTranslatedForPayload := append([]byte(nil), translated...)
	if len(opts.OriginalRequest) > 0 {
		originalTranslatedForPayload, _, _ = helps.TranslateRequestWithAPIKeyModelCompatibilityAndUpdateIntent(ctx, opts.Headers, e.cfg, from, to, baseModel, opts.OriginalRequest, false, isCompat)
	}

	translated, err = helps.ApplyRequestThinking(translated, req, opts, from.String(), to.String(), e.Identifier(), updatesChanged)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	translated, err = helps.PrepareCodeBuddyRequestBody(translated)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	translated = helps.NewPayloadFinalizer(e.cfg, e.Identifier(), baseModel, to.String(), "", originalTranslatedForPayload, req, opts)(translated)
	encoder, err := helps.TokenizerForModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codebuddy executor: tokenizer init failed: %w", err)
	}
	count, err := helps.CountOpenAIChatTokens(encoder, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codebuddy executor: token counting failed: %w", err)
	}

	usageJSON := helps.BuildOpenAIUsageJSON(count)
	return cliproxyexecutor.Response{Payload: sdktranslator.TranslateTokenCount(ctx, to, responseFormat, count, usageJSON)}, nil
}

// Refresh refreshes the CodeBuddy access token. The global gateway issues new
// tokens from the stored refresh token and reports a 401 only after the access
// token has expired.
func (e *CodeBuddyExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debug("codebuddy executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, fmt.Errorf("codebuddy executor: auth is nil")
	}

	identity := codeBuddyIdentity(auth)
	if strings.TrimSpace(identity.RefreshToken) == "" {
		// Nothing to refresh; the credential must be re-authenticated.
		return auth, nil
	}

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 30*time.Second)
	tokenResult, err := codebuddyauth.RefreshToken(ctx, httpClient, identity)
	if err != nil {
		// Surface the upstream status so the conductor can cool down or disable a
		// credential the gateway rejected or rate limited.
		var httpErr *codebuddyauth.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode > 0 {
			return nil, statusErr{code: httpErr.StatusCode, msg: err.Error()}
		}
		return nil, err
	}

	// Work on a copy so a failed persist cannot leave the caller's auth half-updated.
	cloned := auth.Clone()
	if cloned.Metadata == nil {
		cloned.Metadata = make(map[string]any)
	}
	codebuddyauth.ApplyTokenToMetadata(cloned.Metadata, tokenResult, time.Now())
	e.refreshStorage(cloned)

	return cloned, nil
}

// ShouldPrepareRequestAuth reports true when the credential holds a refresh token
// whose access token is about to expire.
func (e *CodeBuddyExecutor) ShouldPrepareRequestAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || cliproxyauth.IsConfigAPIKeyAuth(auth) {
		return false
	}
	identity := codeBuddyIdentity(auth)
	if strings.TrimSpace(identity.RefreshToken) == "" {
		return false
	}
	return identity.NeedsRefresh(time.Now())
}

// PrepareRequestAuth refreshes an expiring access token before request execution so
// the conductor can install and persist the refreshed credential atomically.
func (e *CodeBuddyExecutor) PrepareRequestAuth(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil || !e.ShouldPrepareRequestAuth(auth) {
		return auth, nil
	}
	return e.Refresh(ctx, auth)
}

// refreshStorage rebuilds the credential record from the refreshed metadata. Without
// it a later save would write the expired tokens back to disk. Token fields come from
// the merged metadata so a refresh response that omits refreshToken keeps the stored one.
func (e *CodeBuddyExecutor) refreshStorage(auth *cliproxyauth.Auth) {
	storage, okStorage := auth.Storage.(*codebuddyauth.CodeBuddyTokenStorage)
	if !okStorage || storage == nil {
		return
	}
	identity := codeBuddyIdentity(auth)
	account := &codebuddyauth.Account{
		UID:            identity.UID,
		Nickname:       identity.Nickname,
		EnterpriseID:   identity.EnterpriseID,
		DepartmentInfo: identity.DepartmentInfo,
	}
	merged := &codebuddyauth.TokenResult{
		AccessToken:  identity.AccessToken,
		RefreshToken: identity.RefreshToken,
		ExpiresAtMS:  identity.ExpiresAtMS,
		Domain:       identity.Domain,
	}
	refreshed := codebuddyauth.NewTokenStorage(merged, account, identity.MachineID, identity.Endpoint)
	// Preserve the metadata injected by hooks; the storage serializes it.
	refreshed.Metadata = storage.Metadata
	auth.Storage = refreshed
}

// buildRequestBody translates the client payload, applies the CodeBuddy
// built-in transforms and thinking handling, and finishes with payload config as
// the last body mutation.
func (e *CodeBuddyExecutor) buildRequestBody(
	ctx context.Context,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	baseModel string,
	upstreamModel string,
	to sdktranslator.Format,
) ([]byte, error) {
	from := opts.SourceFormat
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalTranslated := helps.TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, baseModel, bytes.Clone(originalPayloadSource), false)

	// The CodeBuddy gateway only emits SSE, so translation always requests a stream
	// payload even for a non-streaming client request.
	body, err := helps.TranslateRequestReturningError(ctx, opts.Headers, e.cfg, from, to, baseModel, bytes.Clone(req.Payload), true, false)
	if err != nil {
		return nil, err
	}

	body, err = sjson.SetBytes(body, "model", upstreamModel)
	if err != nil {
		return nil, fmt.Errorf("codebuddy executor: failed to set model in payload: %w", err)
	}

	body, err = helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	// Built-in CodeBuddy body transforms run before payload config, which must
	// remain the final semantic step over the business payload.
	body, err = helps.PrepareCodeBuddyRequestBody(body)
	if err != nil {
		return nil, err
	}

	body = helps.NewPayloadFinalizer(e.cfg, e.Identifier(), baseModel, to.String(), "", originalTranslated, req, opts)(body)
	return body, nil
}

// newUpstreamChatRequest builds the upstream HTTP request, including the
// X-Conversation-* headers the global gateway requires.
func (e *CodeBuddyExecutor) newUpstreamChatRequest(ctx context.Context, auth *cliproxyauth.Auth, body []byte, req cliproxyexecutor.Request, originalPayload []byte) (*http.Request, error) {
	identity := codeBuddyIdentity(auth)
	url := codebuddyauth.BuildURL(identity.Endpoint, helps.CodeBuddyChatPath)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, value := range identity.BaseHeaders(true) {
		httpReq.Header.Set(key, value)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	// The seed must come from the request actually sent upstream, so the translated
	// body wins; the pre-translation payload is only a fallback.
	seed := helps.CodeBuddyConversationSeed(body)
	if seed == "" {
		seed = helps.CodeBuddyConversationSeed(originalPayload)
	}
	if seed == "" {
		seed = helps.CodeBuddyConversationSeed(req.Payload)
	}
	httpReq.Header.Set("X-Conversation-ID", codebuddyauth.ConversationID(seed))
	httpReq.Header.Set("X-Conversation-Message-ID", newCodeBuddyUUID())
	httpReq.Header.Set("X-Conversation-Request-ID", newCodeBuddyUUID())

	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
	return httpReq, nil
}

func (e *CodeBuddyExecutor) doUpstream(ctx context.Context, auth *cliproxyauth.Auth, httpReq *http.Request, reporter *helps.UsageReporter) (*http.Response, error) {
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	if reporter != nil {
		httpClient = reporter.TrackHTTPClient(httpClient)
	}
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	return httpResp, nil
}

// codeBuddyIdentity reads the CodeBuddy credential identity from the auth record.
func codeBuddyIdentity(auth *cliproxyauth.Auth) codebuddyauth.Identity {
	if auth == nil {
		return codebuddyauth.IdentityFromMetadata(nil, nil)
	}
	return codebuddyauth.IdentityFromMetadata(auth.Metadata, auth.Attributes)
}

// codeBuddyUpstreamModel returns the model identifier sent upstream.
func codeBuddyUpstreamModel(model string) string {
	return strings.TrimSpace(model)
}

// newCodeBuddyUUID returns a fresh correlation identifier for the
// X-Conversation-* request headers.
func newCodeBuddyUUID() string { return uuid.NewString() }
