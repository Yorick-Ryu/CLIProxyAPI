package registry

import (
	"encoding/json"
	"testing"
)

func TestLunaHeaderOverridesDoNotReturnAfterCatalogRefresh(t *testing.T) {
	old := getModels()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = old
		modelsCatalogStore.mu.Unlock()
	})
	var catalog staticModelsJSON
	if err := json.Unmarshal(embeddedModelsJSON, &catalog); err != nil {
		t.Fatal(err)
	}
	for _, models := range [][]*ModelInfo{catalog.CodexFree, catalog.CodexTeam, catalog.CodexPlus, catalog.CodexPro} {
		for _, model := range models {
			if model.ID == "gpt-5.6-luna" {
				model.Config = &ModelConfig{OverrideHeader: map[string]string{"user-agent": "catalog-client/old", "originator": "catalog-client"}}
			}
		}
	}
	catalog.CodexPro = append(catalog.CodexPro, &ModelInfo{ID: "unrelated-model", Config: &ModelConfig{OverrideHeader: map[string]string{"X-Keep": "preserved"}}})
	raw, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	verify := func() {
		t.Helper()
		got := getModels()
		for _, models := range [][]*ModelInfo{got.CodexFree, got.CodexTeam, got.CodexPlus, got.CodexPro} {
			found := false
			for _, model := range models {
				if model.ID == "gpt-5.6-luna" {
					found = true
					if model.Config != nil && len(model.Config.OverrideHeader) != 0 {
						t.Fatal("Luna catalog restored header overrides")
					}
				}
			}
			if !found {
				t.Fatal("Luna model was removed")
			}
		}
		last := got.CodexPro[len(got.CodexPro)-1]
		if last.Config.OverrideHeader["X-Keep"] != "preserved" {
			t.Fatal("unrelated model override changed")
		}
	}
	if err := loadModelsFromBytes(raw, "fixture"); err != nil {
		t.Fatal(err)
	}
	verify()
	for i := 0; i < 2; i++ {
		changed, err := publishCatalogBytes(raw)
		if err != nil {
			t.Fatal(err)
		}
		verify()
		if len(changed) != 0 {
			t.Fatalf("filtered refresh caused spurious changes: %v", changed)
		}
	}
}
