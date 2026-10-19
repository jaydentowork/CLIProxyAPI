package registry

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestModelThinkingDisableTypeRoundTrip(t *testing.T) {
	for _, handler := range []string{"openai", "claude", "generic"} {
		for _, tc := range []struct {
			name     string
			thinking *ThinkingSupport
		}{
			{name: "absent"},
			{name: "empty", thinking: &ThinkingSupport{}},
			{name: "disable only", thinking: &ThinkingSupport{DisableType: "between_tools"}},
			{name: "all fields", thinking: &ThinkingSupport{
				Min: 1024, Max: 128000, ZeroAllowed: true, DynamicAllowed: true,
				Levels: []string{"low", "high"}, DisableType: "between_tools",
			}},
		} {
			t.Run(handler+"/"+tc.name, func(t *testing.T) {
				registry := &ModelRegistry{}
				payload := registry.convertModelToMap(&ModelInfo{ID: "claude-test", Thinking: tc.thinking}, handler)
				encoded, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				var decoded struct {
					Thinking *ThinkingSupport `json:"thinking"`
				}
				if err := json.Unmarshal(encoded, &decoded); err != nil {
					t.Fatal(err)
				}
				want := tc.thinking
				if tc.name == "empty" {
					want = nil
				}
				if !reflect.DeepEqual(decoded.Thinking, want) {
					t.Fatalf("thinking round trip = %+v, want %+v; payload=%s", decoded.Thinking, want, encoded)
				}
			})
		}
	}
}
