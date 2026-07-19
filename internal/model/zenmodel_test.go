package model

import "testing"

func TestListZenModelsIncludesReleasedModels(t *testing.T) {
	listed := make(map[string]bool)
	for _, item := range ListZenModels() {
		listed[item.ID] = true
	}
	for _, id := range []string{
		"claude-opus-4-6", "gemini-3.1-pro-preview", "gemini-3.1-flash-image-preview",
		"gpt-5.1-codex-mini", "gpt-5.1-codex-max", "gpt-5.3-codex", "gpt-5.6-luna", "gpt-5.6-terra",
		"grok-code-fast", "grok-4.5",
	} {
		if !listed[id] {
			t.Fatalf("released model not exposed: %s", id)
		}
	}
}

func TestCatalogAllowsGatewayAliases(t *testing.T) {
	for _, err := range ValidateCatalog() {
		if err.ModelID == "claude-haiku-4-5-20251001" {
			t.Fatalf("valid gateway alias rejected: %+v", err)
		}
	}
}
