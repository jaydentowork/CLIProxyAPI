package registry

// CodeBuddy (Tencent, global edition, www.codebuddy.ai) chat models.
//
// The catalog is deliberately defined in Go rather than in models.json: the
// embedded catalog is replaced wholesale by the remote catalog refresh, which
// will never carry a codebuddy section, so catalog-backed entries would be
// wiped on the first refresh.
//
// Only chat-capable models are listed. Code completion / next-edit models
// (completion-1.0, completion-1.2, nes-*) and the prompt-enhancement model
// (enhance-1.0) are intentionally excluded.
const codeBuddyOwnedBy = "tencent"

var staticCodeBuddyModels = []*ModelInfo{
	{
		ID:                        "claude-3.7",
		Object:                    "model",
		OwnedBy:                   codeBuddyOwnedBy,
		Type:                      "codebuddy",
		DisplayName:               "Claude 3.7 Sonnet",
		Description:               "CodeBuddy global gateway route to Claude 3.7 Sonnet",
		ContextLength:             200000,
		MaxCompletionTokens:       8192,
		InputTokenLimit:           200000,
		OutputTokenLimit:          8192,
		SupportedInputModalities:  []string{"text", "image"},
		SupportedOutputModalities: []string{"text"},
	},
	{
		ID:                        "claude-4.0",
		Object:                    "model",
		OwnedBy:                   codeBuddyOwnedBy,
		Type:                      "codebuddy",
		DisplayName:               "Claude 4.0 Sonnet",
		Description:               "CodeBuddy global gateway default route to Claude 4.0 Sonnet",
		ContextLength:             200000,
		MaxCompletionTokens:       24000,
		InputTokenLimit:           200000,
		OutputTokenLimit:          24000,
		SupportedInputModalities:  []string{"text", "image"},
		SupportedOutputModalities: []string{"text"},
	},
	{
		ID:                        "gpt-5",
		Object:                    "model",
		OwnedBy:                   codeBuddyOwnedBy,
		Type:                      "codebuddy",
		DisplayName:               "GPT-5",
		Description:               "CodeBuddy global gateway route to GPT-5",
		ContextLength:             272000,
		MaxCompletionTokens:       128000,
		InputTokenLimit:           272000,
		OutputTokenLimit:          128000,
		SupportedInputModalities:  []string{"text", "image"},
		SupportedOutputModalities: []string{"text"},
	},
	{
		ID:                        "gpt-5-mini",
		Object:                    "model",
		OwnedBy:                   codeBuddyOwnedBy,
		Type:                      "codebuddy",
		DisplayName:               "GPT-5 mini",
		Description:               "CodeBuddy global gateway route to GPT-5 mini",
		ContextLength:             272000,
		MaxCompletionTokens:       128000,
		InputTokenLimit:           272000,
		OutputTokenLimit:          128000,
		SupportedInputModalities:  []string{"text", "image"},
		SupportedOutputModalities: []string{"text"},
	},
	{
		ID:                        "gpt-5-nano",
		Object:                    "model",
		OwnedBy:                   codeBuddyOwnedBy,
		Type:                      "codebuddy",
		DisplayName:               "GPT-5 nano",
		Description:               "CodeBuddy global gateway route to GPT-5 nano",
		ContextLength:             272000,
		MaxCompletionTokens:       128000,
		InputTokenLimit:           272000,
		OutputTokenLimit:          128000,
		SupportedInputModalities:  []string{"text", "image"},
		SupportedOutputModalities: []string{"text"},
	},
	{
		ID:                        "o4-mini",
		Object:                    "model",
		OwnedBy:                   codeBuddyOwnedBy,
		Type:                      "codebuddy",
		DisplayName:               "GPT-4o mini",
		Description:               "CodeBuddy global gateway route to GPT-4o mini",
		ContextLength:             200000,
		MaxCompletionTokens:       32000,
		InputTokenLimit:           200000,
		OutputTokenLimit:          32000,
		SupportedInputModalities:  []string{"text", "image"},
		SupportedOutputModalities: []string{"text"},
	},
	{
		ID:                        "chat-1.0",
		Object:                    "model",
		OwnedBy:                   codeBuddyOwnedBy,
		Type:                      "codebuddy",
		DisplayName:               "CodeBuddy chat-1.0",
		Description:               "CodeBuddy hosted chat model with tool calling",
		ContextLength:             128000,
		MaxCompletionTokens:       32000,
		InputTokenLimit:           128000,
		OutputTokenLimit:          32000,
		SupportedInputModalities:  []string{"text"},
		SupportedOutputModalities: []string{"text"},
	},
	{
		ID:                        "gemini-2.5-flash",
		Object:                    "model",
		OwnedBy:                   codeBuddyOwnedBy,
		Type:                      "codebuddy",
		DisplayName:               "Gemini 2.5 Flash",
		Description:               "CodeBuddy global gateway route to Gemini 2.5 Flash",
		ContextLength:             1000000,
		MaxCompletionTokens:       16384,
		InputTokenLimit:           1000000,
		OutputTokenLimit:          16384,
		SupportedInputModalities:  []string{"text", "image"},
		SupportedOutputModalities: []string{"text"},
	},
	{
		ID:                        "gemini-2.5-pro",
		Object:                    "model",
		OwnedBy:                   codeBuddyOwnedBy,
		Type:                      "codebuddy",
		DisplayName:               "Gemini 2.5 Pro",
		Description:               "CodeBuddy global gateway route to Gemini 2.5 Pro",
		ContextLength:             1000000,
		MaxCompletionTokens:       16384,
		InputTokenLimit:           1000000,
		OutputTokenLimit:          16384,
		SupportedInputModalities:  []string{"text", "image"},
		SupportedOutputModalities: []string{"text"},
	},
	{
		ID:                        "auto-chat",
		Object:                    "model",
		OwnedBy:                   codeBuddyOwnedBy,
		Type:                      "codebuddy",
		DisplayName:               "CodeBuddy auto",
		Description:               "CodeBuddy model router that picks a model per request",
		ContextLength:             32000,
		MaxCompletionTokens:       8192,
		InputTokenLimit:           32000,
		OutputTokenLimit:          8192,
		SupportedInputModalities:  []string{"text"},
		SupportedOutputModalities: []string{"text"},
	},
}

// GetCodeBuddyModels returns the CodeBuddy chat model catalog. Every call
// returns independent copies so callers cannot mutate the shared definitions.
func GetCodeBuddyModels() []*ModelInfo {
	return cloneModelInfos(staticCodeBuddyModels)
}
