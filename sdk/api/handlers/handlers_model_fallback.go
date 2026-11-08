package handlers

import (
	"bytes"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

const (
	modelFallbackKindRefusal       = "refusal"
	modelFallbackKindContentFilter = "content_filter"
	// modelFallbackKindFiltered is a translated finish/incomplete reason of content_filter, which
	// protocol translators also emit for upstream refusals, so it matches either trigger.
	modelFallbackKindFiltered = "filtered"

	// modelFallbackMaxBufferedChunks and modelFallbackMaxBufferedBytes cap the stream preamble
	// held back while a guarded attempt waits for its first substantive chunk or a refusal.
	modelFallbackMaxBufferedChunks = 16
	modelFallbackMaxBufferedBytes  = 64 << 10

	modelFallbackDefaultSessionTTL = time.Hour
	// modelFallbackMaxSessionTTL is also the LCP matcher TTL, so a pin never outlives the
	// prefix group that derives its headerless session identity.
	modelFallbackMaxSessionTTL = 24 * time.Hour
	modelFallbackLCPAuthID     = "model-fallback"
)

var modelFallbackContentFilterErrorTexts = []string{
	"request blocked by safety filter",
	"content_filter",
	"content management policy",
	"output blocked by content filtering",
}

var modelFallbackGeminiSafetyReasons = map[string]struct{}{
	"SAFETY":             {},
	"PROHIBITED_CONTENT": {},
	"BLOCKLIST":          {},
	"SPII":               {},
	"IMAGE_SAFETY":       {},
}

type modelFallbackPin struct {
	model     string
	ttl       time.Duration
	expiresAt time.Time
}

// modelFallbackState stores session pins and the headerless session matcher.
type modelFallbackState struct {
	mu      sync.Mutex
	nowFunc func() time.Time
	pins    map[string]modelFallbackPin
	matcher *coresession.MerklePrefixMatcher
}

func newModelFallbackState(nowFunc func() time.Time) *modelFallbackState {
	if nowFunc == nil {
		nowFunc = time.Now
	}
	state := &modelFallbackState{nowFunc: nowFunc, pins: make(map[string]modelFallbackPin)}
	state.matcher = coresession.NewMerklePrefixMatcherWithConfig(coresession.MerklePrefixMatcherConfig{
		TTL:     modelFallbackMaxSessionTTL,
		NowFunc: state.now,
	})
	return state
}

func (s *modelFallbackState) now() time.Time {
	return s.nowFunc()
}

// lookup returns the live pin for key. It never extends the pin: only a successful
// fallback attempt re-pins, so a pinned model that starts failing lapses within one TTL.
func (s *modelFallbackState) lookup(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pin, ok := s.pins[key]
	if !ok {
		return "", false
	}
	if !s.now().Before(pin.expiresAt) {
		delete(s.pins, key)
		return "", false
	}
	return pin.model, true
}

// pin records model for key and sweeps expired pins.
func (s *modelFallbackState) pin(key, model string, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for existingKey, existing := range s.pins {
		if !now.Before(existing.expiresAt) {
			delete(s.pins, existingKey)
		}
	}
	s.pins[key] = modelFallbackPin{model: model, ttl: ttl, expiresAt: now.Add(ttl)}
}

func (h *BaseAPIHandler) modelFallbackState() *modelFallbackState {
	h.runtimeMu.RLock()
	state := h.modelFallback
	h.runtimeMu.RUnlock()
	if state != nil {
		return state
	}
	h.runtimeMu.Lock()
	defer h.runtimeMu.Unlock()
	if h.modelFallback == nil {
		h.modelFallback = newModelFallbackState(nil)
	}
	return h.modelFallback
}

// modelFallbackPlan is the ordered candidate list for one request.
type modelFallbackPlan struct {
	rule       config.ModelFallbackRule
	candidates []string
	start      int
	state      *modelFallbackState
	pinKey     string
	ttl        time.Duration
}

// planModelFallback returns nil when no fallback rule matches the requested model.
func (h *BaseAPIHandler) planModelFallback(ctx context.Context, entryProtocol, modelName string, rawJSON []byte, execOptions modelExecutionOptions) *modelFallbackPlan {
	cfg := h.CurrentConfig()
	if cfg == nil || len(cfg.Routing.ModelFallbacks) == 0 {
		return nil
	}
	for _, rule := range cfg.Routing.ModelFallbacks {
		if !matchModelFallbackPattern(rule.From, modelName) {
			continue
		}
		candidates := modelFallbackCandidates(modelName, rule)
		if len(candidates) < 2 {
			return nil
		}
		plan := &modelFallbackPlan{
			rule:       rule,
			candidates: candidates,
			state:      h.modelFallbackState(),
			ttl:        modelFallbackSessionTTL(rule, cfg.Routing.SessionAffinityTTL),
		}
		if rule.StickySession != nil && !*rule.StickySession {
			return plan
		}
		sessionID := h.modelFallbackSessionID(ctx, entryProtocol, modelName, rawJSON, execOptions, plan.state)
		if sessionID == "" {
			return plan
		}
		plan.pinKey = sessionID + "::" + baseModelKey(modelName)
		if pinned, ok := plan.state.lookup(plan.pinKey); ok {
			for index := 1; index < len(candidates); index++ {
				if modelFallbackNormalize(candidates[index]) == modelFallbackNormalize(pinned) {
					plan.start = index
					break
				}
			}
		}
		return plan
	}
	return nil
}

func (p *modelFallbackPlan) pin(model string) {
	if p == nil || p.pinKey == "" {
		return
	}
	p.state.pin(p.pinKey, model, p.ttl)
}

// advance logs the retry after candidates[index] was refused or filtered. The next
// candidate is pinned only once it succeeds, so a broken fallback never sticks.
func (p *modelFallbackPlan) advance(index int) {
	log.WithFields(log.Fields{
		"model":          p.candidates[index],
		"fallback_model": p.candidates[index+1],
	}).Info("model fallback: refusal or content filter, retrying with fallback model")
}

// modelFallbackSessionID resolves the conversation identity used for pinning.
// Explicit client sessions win; otherwise a caller-scoped LCP prefix group keeps
// multi-turn headerless conversations stable; otherwise the canonical hash is used.
func (h *BaseAPIHandler) modelFallbackSessionID(ctx context.Context, entryProtocol, modelName string, rawJSON []byte, execOptions modelExecutionOptions, state *modelFallbackState) string {
	headers := modelExecutionHeaders(ctx, execOptions.Headers)
	meta := requestExecutionMetadata(ctx)
	addCallerScopeMetadata(meta, execOptions.CallerScope)
	if info, ok := coresession.ExtractSessionInfo(headers, rawJSON, maps.Clone(meta)); ok && info.SessionID != "" && info.ClientType != "lcp" {
		return coreauth.CanonicalSessionID(headers, rawJSON, maps.Clone(meta))
	}
	if callerScope := callerScopeFromMetadata(meta); callerScope != "" {
		namespace := callerScope + "::" + baseModelKey(modelName)
		if turns := coresession.ExtractCanonicalTurns(sdktranslator.FromString(entryProtocol), rawJSON); len(turns) > 0 {
			if sessionID := strings.TrimSpace(state.matcher.BindWithResult(namespace, turns, modelFallbackLCPAuthID).SessionID); sessionID != "" {
				return sessionID
			}
		}
	}
	req, opts := coresession.Enrich(
		coreexecutor.Request{Model: modelName, Payload: rawJSON},
		coreexecutor.Options{Headers: headers, Metadata: maps.Clone(meta), SourceFormat: sdktranslator.FromString(entryProtocol)},
	)
	return coreauth.CanonicalSessionID(opts.Headers, req.Payload, maps.Clone(opts.Metadata))
}

func modelFallbackSessionTTL(rule config.ModelFallbackRule, routingTTL string) time.Duration {
	ttl := modelFallbackDefaultSessionTTL
	for _, raw := range []string{rule.SessionTTL, routingTTL} {
		if parsed, errParse := time.ParseDuration(strings.TrimSpace(raw)); errParse == nil && parsed > 0 {
			ttl = parsed
			break
		}
	}
	if ttl > modelFallbackMaxSessionTTL {
		ttl = modelFallbackMaxSessionTTL
	}
	return ttl
}

func modelFallbackCandidates(modelName string, rule config.ModelFallbackRule) []string {
	candidates := []string{modelName}
	seen := map[string]struct{}{modelFallbackNormalize(modelName): {}}
	for _, target := range append(append([]string(nil), rule.To...), rule.Fallback) {
		target = strings.TrimSpace(target)
		key := modelFallbackNormalize(target)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		candidates = append(candidates, target)
	}
	return candidates
}

func modelFallbackNormalize(model string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(model)), ".", "-")
}

// baseModelKey strips a thinking suffix and normalizes case and separators.
func baseModelKey(model string) string {
	model = strings.TrimSpace(model)
	if base := strings.TrimSpace(thinking.ParseSuffix(model).ModelName); base != "" {
		model = base
	}
	return modelFallbackNormalize(model)
}

func matchModelFallbackPattern(pattern, model string) bool {
	normalizedPattern := modelFallbackNormalize(pattern)
	if normalizedPattern == "" {
		return false
	}
	normalizedModel := modelFallbackNormalize(model)
	modelBase := baseModelKey(model)
	if strings.Contains(normalizedPattern, "*") {
		return modelFallbackGlob(normalizedPattern, normalizedModel) || modelFallbackGlob(normalizedPattern, modelBase)
	}
	return normalizedPattern == normalizedModel || normalizedPattern == modelBase || baseModelKey(pattern) == modelBase
}

func modelFallbackGlob(pattern, value string) bool {
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}
	value = value[len(parts[0]):]
	for _, part := range parts[1 : len(parts)-1] {
		index := strings.Index(value, part)
		if index < 0 {
			return false
		}
		value = value[index+len(part):]
	}
	return strings.HasSuffix(value, parts[len(parts)-1])
}

// modelFallbackAttemptInput rewrites the request for a fallback candidate. The prepared
// stream route belongs to the original model, so it is hidden to force re-routing.
func modelFallbackAttemptInput(ctx context.Context, model, originalModel string, rawJSON []byte) (context.Context, []byte) {
	if model == originalModel {
		return ctx, rawJSON
	}
	ctx = context.WithValue(ctx, preparedModelRouteContextKey{}, nil)
	if original := originalRequestFromContext(ctx); len(original) > 0 {
		ctx = WithOriginalRequest(ctx, rewriteModelFallbackBody(original, model))
	}
	return ctx, rewriteModelFallbackBody(rawJSON, model)
}

func rewriteModelFallbackBody(raw []byte, model string) []byte {
	if len(raw) == 0 || !ojson.GetBytesNoCopy(raw, "model").Exists() {
		return raw
	}
	doc := ojson.NewDoc(bytes.Clone(raw))
	if errSet := doc.Set("model", model); errSet != nil {
		log.Debugf("model fallback: rewrite request model: %v", errSet)
		return raw
	}
	return doc.Bytes()
}

func modelFallbackRuleAllows(rule config.ModelFallbackRule, kind string) bool {
	if kind == "" {
		return false
	}
	if len(rule.On) == 0 {
		return true
	}
	for _, on := range rule.On {
		normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(on)), "-", "_")
		if normalized == kind || normalized == "any" || normalized == "all" {
			return true
		}
		if kind == modelFallbackKindFiltered && (normalized == modelFallbackKindRefusal || normalized == modelFallbackKindContentFilter) {
			return true
		}
	}
	return false
}

func isModelFallbackTriggerError(err error, rule config.ModelFallbackRule) bool {
	if err == nil {
		return false
	}
	return modelFallbackRuleAllows(rule, modelFallbackErrorKind(err))
}

func modelFallbackErrorKind(err error) string {
	if clienterror.IsClaudePolicyRefusalError(err) {
		return modelFallbackKindRefusal
	}
	if clienterror.IsGeminiFilterBlockError(err) {
		return modelFallbackKindContentFilter
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "claude refusal") {
		return modelFallbackKindRefusal
	}
	for _, text := range modelFallbackContentFilterErrorTexts {
		if strings.Contains(message, text) {
			return modelFallbackKindContentFilter
		}
	}
	return ""
}

// isModelFallbackTriggerPayload detects HTTP 200 refusals and content filter results.
func isModelFallbackTriggerPayload(payload []byte, responseProtocol string, rule config.ModelFallbackRule) bool {
	kind := modelFallbackPayloadKind(payload)
	if kind == "" && modelFallbackIsSSEProtocol(responseProtocol, payload) {
		return isModelFallbackTriggerStreamChunk(payload, responseProtocol, rule)
	}
	return modelFallbackRuleAllows(rule, kind)
}

// isModelFallbackTriggerStreamChunk detects refusal and content filter events in a stream chunk.
func isModelFallbackTriggerStreamChunk(payload []byte, _ string, rule config.ModelFallbackRule) bool {
	for _, event := range modelFallbackStreamEvents(payload) {
		if modelFallbackRuleAllows(rule, modelFallbackPayloadKind(event)) {
			return true
		}
	}
	return false
}

func modelFallbackIsSSEProtocol(_ string, payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	return bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte("data:"))
}

func modelFallbackPayloadKind(payload []byte) string {
	if len(bytes.TrimSpace(payload)) == 0 || !ojson.ValidBytes(payload) {
		return ""
	}
	if _, ok := clienterror.ClaudePolicyRefusalMessage(payload); ok {
		return modelFallbackKindRefusal
	}
	if _, ok := clienterror.GeminiFilterBlockMessage(payload); ok {
		return modelFallbackKindContentFilter
	}
	for _, path := range []string{"stop_reason", "delta.stop_reason"} {
		switch strings.ToLower(strings.TrimSpace(ojson.GetBytesNoCopy(payload, path).String())) {
		case "refusal", "sensitive":
			return modelFallbackKindRefusal
		}
	}
	for _, path := range []string{"choices.0.finish_reason", "incomplete_details.reason", "response.incomplete_details.reason"} {
		if strings.ToLower(strings.TrimSpace(ojson.GetBytesNoCopy(payload, path).String())) == "content_filter" {
			return modelFallbackKindFiltered
		}
	}
	for _, path := range []string{"candidates.0.finishReason", "response.candidates.0.finishReason", "promptFeedback.blockReason", "response.promptFeedback.blockReason"} {
		if _, ok := modelFallbackGeminiSafetyReasons[strings.ToUpper(strings.TrimSpace(ojson.GetBytesNoCopy(payload, path).String()))]; ok {
			return modelFallbackKindContentFilter
		}
	}
	return ""
}

// modelFallbackStreamEvents returns the JSON documents carried by a raw JSON or SSE chunk.
func modelFallbackStreamEvents(payload []byte) [][]byte {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return [][]byte{trimmed}
	}
	normalized := bytes.ReplaceAll(trimmed, []byte("\r\n"), []byte("\n"))
	var events [][]byte
	for _, frame := range bytes.Split(normalized, []byte("\n\n")) {
		data, found := sseJSONValidationDataPayload(frame)
		if data = bytes.TrimSpace(data); found && len(data) > 0 {
			events = append(events, data)
		}
	}
	return events
}

// isModelFallbackPreambleChunk reports whether a chunk carries no model output yet:
// message_start, ping, response.created/in_progress, role-only deltas, SSE comments,
// and empty non-tool content_block_start frames.
func isModelFallbackPreambleChunk(payload []byte) bool {
	events := modelFallbackStreamEvents(payload)
	if len(events) == 0 {
		for _, line := range bytes.Split(payload, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) > 0 && !bytes.HasPrefix(line, []byte(":")) {
				return false
			}
		}
		return true
	}
	for _, event := range events {
		if !isModelFallbackPreambleEvent(event) {
			return false
		}
	}
	return true
}

func isModelFallbackPreambleEvent(event []byte) bool {
	if !ojson.ValidBytes(event) {
		return false
	}
	switch ojson.GetBytesNoCopy(event, "type").String() {
	case "message_start", "ping", "response.created", "response.in_progress":
		return true
	case "content_block_start":
		block := ojson.GetBytesNoCopy(event, "content_block")
		return block.Get("name").String() == "" && block.Get("text").String() == "" && block.Get("thinking").String() == ""
	case "response.output_item.added":
		item := ojson.GetBytesNoCopy(event, "item")
		return item.Get("type").String() != "function_call" && item.Get("name").String() == "" &&
			!item.Get("content.0").Exists() && !item.Get("summary.0").Exists()
	case "response.content_part.added":
		return ojson.GetBytesNoCopy(event, "part.text").String() == ""
	}
	choice := ojson.GetBytesNoCopy(event, "choices.0")
	if !choice.Exists() || choice.Get("finish_reason").String() != "" {
		return false
	}
	delta := choice.Get("delta")
	if !delta.Exists() {
		return false
	}
	return delta.Get("content").String() == "" &&
		delta.Get("reasoning_content").String() == "" &&
		delta.Get("refusal").String() == "" &&
		!delta.Get("tool_calls").Exists() &&
		!delta.Get("function_call").Exists()
}

// modelFallbackStreamGuard tracks one stream candidate. Non-terminal attempts hold their
// preamble until the first substantive chunk and are abandoned on a refusal or content
// filter. The terminal attempt delivers refusals as-is but still reports them so the
// session is never pinned to a refusing model.
type modelFallbackStreamGuard struct {
	rule     config.ModelFallbackRule
	release  context.CancelFunc
	terminal bool
	// refused reports a refusal or content filter seen during bootstrap.
	refused bool
	// delivering means the delivery goroutine owns release.
	delivering bool
	// detached means the attempt owns its context (plugin executor route).
	detached bool
	// committed means delivery started after a successful bootstrap.
	committed bool
}

// abandon cancels a refused attempt and drains its upstream channel so the producer exits.
func (g *modelFallbackStreamGuard) abandon(chunks <-chan coreexecutor.StreamChunk) {
	g.refused = true
	g.release()
	if chunks == nil {
		return
	}
	go func() {
		for range chunks {
		}
	}()
}
