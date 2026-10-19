package signature

import (
	"strings"
	"testing"
)

// The Meta envelope probe must coexist with fork-only SWE recognition and
// specific Claude generation diagnostics instead of flattening detection results.
func TestProviderDetectionMetaPreservesForkDiagnostics(t *testing.T) {
	meta := testMetaReasoningSignature(1024)
	for _, sig := range []string{meta, meta + "." + testMetaRouterSuffix()} {
		detection := detectSignatureProviderForBlock(sig, SignatureBlockKindClaudeThinking)
		if detection.provider != SignatureProviderMeta || detection.rejectionReason != "" {
			t.Fatalf("Meta detection = %+v, want Meta without a Claude rejection", detection)
		}
		decision := DecideSignatureCompatibility(SignatureProviderClaude, sig, SignatureBlockKindClaudeThinking)
		if decision.Compatible || decision.Action != SignatureActionDropBlock || decision.DetectedProvider != SignatureProviderMeta {
			t.Fatalf("Claude decision for Meta = %+v, want foreign block dropped", decision)
		}
	}

	parts := defaultClaudeModelFreeCAISParts()
	parts.channelID = 19
	for _, sig := range []string{parts.encode(), "claude#" + parts.encode()} {
		detection := detectSignatureProviderForBlock(sig, SignatureBlockKindClaudeThinking)
		if detection.provider != SignatureProviderUnknown || !strings.Contains(detection.rejectionReason, "unknown channel_id 19") {
			t.Fatalf("unknown Claude generation detection = %+v, want precise rejection", detection)
		}
	}

	for _, sig := range []string{"sealed.v1.merge-regression", "swe#sealed.v1.merge-regression"} {
		if got := DetectSignatureProvider(sig); got != SignatureProviderSWE {
			t.Fatalf("SWE detection = %q, want %q", got, SignatureProviderSWE)
		}
	}
}
