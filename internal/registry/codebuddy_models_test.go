package registry

import "testing"

// TestGetCodeBuddyModelsSurvivesCatalogSwap guards the reason the CodeBuddy
// catalog lives in Go instead of models.json: the remote catalog refresh
// replaces the embedded catalog wholesale and never carries a codebuddy
// section, so a catalog-backed list would be wiped on the first refresh.
func TestGetCodeBuddyModelsSurvivesCatalogSwap(t *testing.T) {
	original := getModels()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = original
		modelsCatalogStore.mu.Unlock()
	})

	if err := loadModelsFromBytes([]byte(`{"claude":[{"id":"claude-swap-probe"}]}`), "test"); err != nil {
		t.Fatalf("loadModelsFromBytes: %v", err)
	}

	models := GetCodeBuddyModels()
	if len(models) == 0 {
		t.Fatal("GetCodeBuddyModels() returned no models after a catalog swap")
	}
	if byChannel := GetStaticModelDefinitionsByChannel("codebuddy"); len(byChannel) != len(models) {
		t.Fatalf("GetStaticModelDefinitionsByChannel(codebuddy) returned %d models, want %d", len(byChannel), len(models))
	}
	if info := LookupStaticModelInfo("chat-1.0"); info == nil {
		t.Fatal("LookupStaticModelInfo(chat-1.0) = nil after a catalog swap")
	}

	// Otherwise model IDs shared with other providers keep resolving to their
	// own catalog entry.
	if info := LookupStaticModelInfo("claude-swap-probe"); info == nil {
		t.Fatal("LookupStaticModelInfo(claude-swap-probe) = nil after a catalog swap")
	}
}

func TestGetCodeBuddyModelsExcludesNonChatModels(t *testing.T) {
	models := GetCodeBuddyModels()
	if len(models) == 0 {
		t.Fatal("GetCodeBuddyModels() returned no models")
	}

	wantIDs := map[string]bool{
		"claude-3.7":       false,
		"claude-4.0":       false,
		"gpt-5":            false,
		"gpt-5-mini":       false,
		"gpt-5-nano":       false,
		"o4-mini":          false,
		"chat-1.0":         false,
		"gemini-2.5-flash": false,
		"gemini-2.5-pro":   false,
		"auto-chat":        false,
	}
	excluded := map[string]bool{
		"completion-1.0": true,
		"completion-1.2": true,
		"nes-1.0":        true,
		"nes-1.1":        true,
		"nes-1.2":        true,
		"nes-v1-14b":     true,
		"enhance-1.0":    true,
	}

	for _, model := range models {
		if model == nil {
			t.Fatal("GetCodeBuddyModels() contains a nil model")
		}
		if excluded[model.ID] {
			t.Errorf("non-chat model %q must not be listed", model.ID)
		}
		if _, ok := wantIDs[model.ID]; ok {
			wantIDs[model.ID] = true
		}
		if model.OwnedBy != codeBuddyOwnedBy || model.Type != "codebuddy" {
			t.Errorf("model %q owned_by/type = %s/%s, want tencent/codebuddy", model.ID, model.OwnedBy, model.Type)
		}
	}
	for id, found := range wantIDs {
		if !found {
			t.Errorf("expected CodeBuddy model %q to be listed", id)
		}
	}
	if len(models) != len(wantIDs) {
		t.Errorf("GetCodeBuddyModels() returned %d models, want %d", len(models), len(wantIDs))
	}

	// The shared definitions must not be mutable through the returned copies.
	models[0].ID = "mutated"
	if GetCodeBuddyModels()[0].ID == "mutated" {
		t.Fatal("GetCodeBuddyModels() returned the shared static definitions instead of copies")
	}
}
