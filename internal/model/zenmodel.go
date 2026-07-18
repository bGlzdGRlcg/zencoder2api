package model

import (
	"sort"
)

// ThinkingConfig thinking模式配置
type ThinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budgetTokens"`
	Signature    string `json:"signature,omitempty"`
}

// ReasoningConfig OpenAI reasoning配置
type ReasoningConfig struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary,omitempty"`
}

// TextConfig OpenAI text配置
type TextConfig struct {
	Verbosity string `json:"verbosity"`
}

// ModelParameters 模型参数配置
type ModelParameters struct {
	Temperature     *float64          `json:"temperature,omitempty"`
	Thinking        *ThinkingConfig   `json:"thinking,omitempty"`
	Reasoning       *ReasoningConfig  `json:"reasoning,omitempty"`
	Effort          string            `json:"effort,omitempty"`
	Text            *TextConfig       `json:"text,omitempty"`
	ExtraHeaders    map[string]string `json:"extraHeaders,omitempty"`
	ForceStreaming  *bool             `json:"forceStreaming,omitempty"`
	ThinkingBudget  int               `json:"thinkingBudget,omitempty"`
	ServiceTier     string            `json:"serviceTier,omitempty"`
	MaxInputChars   int               `json:"maxInputChars,omitempty"`
	MaxOutputTokens int               `json:"maxOutputTokens,omitempty"`
}

type ZenModel struct {
	ID          string           `json:"id"`
	GatewayID   string           `json:"-"` // Internal catalog ID used in zen-model-id.
	DisplayName string           `json:"displayName"`
	Model       string           `json:"model"`
	Multiplier  float64          `json:"multiplier"`
	ProviderID  string           `json:"providerId"`
	Parameters  *ModelParameters `json:"parameters,omitempty"`
	IsHidden    bool             `json:"isHidden"`
}

// 辅助变量
var (
	temp0 = 0.0
	temp1 = 1.0

	// OpenAI reasoning参数
	openaiParams = &ModelParameters{
		Reasoning: &ReasoningConfig{Effort: "medium"},
		Text:      &TextConfig{Verbosity: "medium"},
	}

	anthropicEnabledParams = &ModelParameters{
		Temperature: &temp1,
		Thinking:    &ThinkingConfig{Type: "enabled", BudgetTokens: 4096},
		ExtraHeaders: map[string]string{
			"anthropic-beta": "interleaved-thinking-2025-05-14",
		},
	}
	anthropicAdaptiveParams = &ModelParameters{
		Thinking: &ThinkingConfig{Type: "adaptive"},
		ExtraHeaders: map[string]string{
			"anthropic-beta": "interleaved-thinking-2025-05-14",
		},
	}
	anthropicAdaptiveNoBetaParams = &ModelParameters{
		Thinking: &ThinkingConfig{Type: "adaptive"},
	}
	minimaxParams = &ModelParameters{Temperature: &temp1}
	glmParams     = &ModelParameters{Temperature: &temp1}
)

// 模型映射表（同步自 Zencoder gateway models v2）
var ZenModels = map[string]ZenModel{
	// Anthropic
	"claude-haiku-4-5":          {ID: "claude-haiku-4-5", GatewayID: "haiku-4-5-think", DisplayName: "Claude Haiku 4.5", Model: "claude-haiku-4-5-20251001", Multiplier: 1, ProviderID: "anthropic", Parameters: anthropicEnabledParams},
	"claude-haiku-4-5-20251001": {ID: "claude-haiku-4-5-20251001", GatewayID: "haiku-4-5-think", DisplayName: "Claude Haiku 4.5", Model: "claude-haiku-4-5-20251001", Multiplier: 1, ProviderID: "anthropic", Parameters: anthropicEnabledParams},
	"claude-sonnet-4-6":         {ID: "claude-sonnet-4-6", GatewayID: "sonnet-4-6-think", DisplayName: "Claude Sonnet 4.6", Model: "claude-sonnet-4-6", Multiplier: 3, ProviderID: "anthropic", Parameters: anthropicAdaptiveParams},
	"claude-sonnet-5":           {ID: "claude-sonnet-5", GatewayID: "sonnet-5-think", DisplayName: "Claude Sonnet 5", Model: "claude-sonnet-5", Multiplier: 3, ProviderID: "anthropic", Parameters: anthropicAdaptiveNoBetaParams},
	"claude-opus-4-6":           {ID: "claude-opus-4-6", GatewayID: "opus-4-6-think", DisplayName: "Claude Opus 4.6", Model: "claude-opus-4-6", Multiplier: 5, ProviderID: "anthropic", Parameters: anthropicAdaptiveParams, IsHidden: true},
	"claude-opus-4-7":           {ID: "claude-opus-4-7", GatewayID: "opus-4-7-think", DisplayName: "Claude Opus 4.7", Model: "claude-opus-4-7", Multiplier: 5, ProviderID: "anthropic", Parameters: anthropicAdaptiveNoBetaParams},
	"claude-opus-4-8":           {ID: "claude-opus-4-8", GatewayID: "opus-4-8-think", DisplayName: "Claude Opus 4.8", Model: "claude-opus-4-8", Multiplier: 5, ProviderID: "anthropic", Parameters: anthropicAdaptiveNoBetaParams},

	// Gemini
	"gemini-3.1-pro-preview":             {ID: "gemini-3.1-pro-preview", GatewayID: "gemini-3-1-pro-preview", DisplayName: "Gemini Pro 3.1", Model: "gemini-3.1-pro-preview", Multiplier: 2, ProviderID: "gemini", IsHidden: true},
	"gemini-3.1-pro-preview-customtools": {ID: "gemini-3.1-pro-preview-customtools", GatewayID: "gemini-3-1-pro-preview-customtools", DisplayName: "Gemini Pro 3.1", Model: "gemini-3.1-pro-preview-customtools", Multiplier: 2, ProviderID: "gemini"},
	"gemini-3-flash-preview":             {ID: "gemini-3-flash-preview", DisplayName: "Gemini Flash 3.0", Model: "gemini-3-flash-preview", Multiplier: 1, ProviderID: "gemini"},
	"gemini-3.5-flash":                   {ID: "gemini-3.5-flash", GatewayID: "gemini-3-5-flash", DisplayName: "Gemini Flash 3.5", Model: "gemini-3.5-flash", Multiplier: 2.5, ProviderID: "gemini"},
	"gemini-3.1-flash-image-preview":     {ID: "gemini-3.1-flash-image-preview", GatewayID: "gemini-3-1-flash-image-preview", DisplayName: "Gemini 3.1 Flash Image Preview", Model: "gemini-3.1-flash-image-preview", Multiplier: 8, ProviderID: "gemini", IsHidden: true},

	// Other gateway providers exposed through the OpenAI-compatible route
	"minimax-m3": {ID: "minimax-m3", DisplayName: "MiniMax M3", Model: "accounts/fireworks/models/minimax-m3", Multiplier: 1, ProviderID: "minimax", Parameters: minimaxParams},
	"glm-5.2":    {ID: "glm-5.2", GatewayID: "glm-5-2", DisplayName: "GLM 5.2", Model: "accounts/fireworks/models/glm-5p2", Multiplier: 2, ProviderID: "glm", Parameters: glmParams},

	// OpenAI
	"gpt-5.1-codex-mini": {ID: "gpt-5.1-codex-mini", GatewayID: "gpt-5-1-codex-mini", DisplayName: "GPT-5.1 Codex mini", Model: "gpt-5.1-codex-mini", Multiplier: 0.5, ProviderID: "openai", Parameters: openaiParams, IsHidden: true},
	"gpt-5.1-codex-max":  {ID: "gpt-5.1-codex-max", GatewayID: "gpt-5-1-codex-max", DisplayName: "GPT-5.1 Codex Max", Model: "gpt-5.1-codex-max", Multiplier: 1.5, ProviderID: "openai", Parameters: openaiParams, IsHidden: true},
	"gpt-5.3-codex":      {ID: "gpt-5.3-codex", GatewayID: "gpt-5-3-codex", DisplayName: "GPT-5.3 Codex", Model: "gpt-5.3-codex", Multiplier: 2, ProviderID: "openai", Parameters: openaiParams, IsHidden: true},
	"gpt-5.4-mini":       {ID: "gpt-5.4-mini", GatewayID: "gpt-5-4-mini", DisplayName: "GPT-5.4 mini", Model: "gpt-5.4-mini", Multiplier: 1.25, ProviderID: "openai", Parameters: openaiParams},
	"gpt-5.4":            {ID: "gpt-5.4", GatewayID: "gpt-5-4", DisplayName: "GPT-5.4", Model: "gpt-5.4", Multiplier: 2.5, ProviderID: "openai", Parameters: openaiParams},
	"gpt-5.5":            {ID: "gpt-5.5", GatewayID: "gpt-5-5", DisplayName: "GPT-5.5", Model: "gpt-5.5", Multiplier: 5, ProviderID: "openai", Parameters: openaiParams},
	"gpt-5.6-luna":       {ID: "gpt-5.6-luna", GatewayID: "gpt-5-6-luna", DisplayName: "GPT-5.6 Luna", Model: "gpt-5.6-luna", Multiplier: 1.25, ProviderID: "openai", Parameters: openaiParams, IsHidden: true},
	"gpt-5.6-terra":      {ID: "gpt-5.6-terra", GatewayID: "gpt-5-6-terra", DisplayName: "GPT-5.6 Terra", Model: "gpt-5.6-terra", Multiplier: 2.5, ProviderID: "openai", Parameters: openaiParams, IsHidden: true},

	// xAI
	"grok-code-fast": {ID: "grok-code-fast", DisplayName: "Grok Code Fast 1", Model: "grok-code-fast-1", Multiplier: 0.25, ProviderID: "xai", Parameters: &ModelParameters{Temperature: &temp0}, IsHidden: true},
	"grok-4.5":       {ID: "grok-4.5", GatewayID: "grok-4-5", DisplayName: "Grok 4.5", Model: "grok-4.5", Multiplier: 2, ProviderID: "xai", Parameters: &ModelParameters{Reasoning: &ReasoningConfig{Effort: "high"}}, IsHidden: true},
}

// GetZenModel 获取模型配置，如果不存在则返回空模型和false
func GetZenModel(modelID string) (ZenModel, bool) {
	if m, ok := ZenModels[modelID]; ok {
		return m, true
	}

	// Third-party OpenAI clients often send the gateway's actual model name
	// instead of the public Zencoder model ID. Treat both names as aliases so
	// provider-specific parameters (especially reasoning) are still applied.
	for _, m := range ZenModels {
		if m.Model == modelID {
			return m, true
		}
	}

	return ZenModel{}, false
}

// ListZenModels returns the canonical public model IDs exposed by the proxy.
func ListZenModels() []ZenModel {
	seen := make(map[string]struct{}, len(ZenModels))
	models := make([]ZenModel, 0, len(ZenModels))
	for _, zenModel := range ZenModels {
		if zenModel.ID == "" {
			continue
		}
		if _, exists := seen[zenModel.ID]; exists {
			continue
		}
		seen[zenModel.ID] = struct{}{}
		models = append(models, zenModel)
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})
	return models
}

// ResolveOpenAIModel applies neutral OpenAI defaults when callers send a model
// name that is not present in the local catalog.
func ResolveOpenAIModel(modelID string) ZenModel {
	if m, ok := GetZenModel(modelID); ok {
		return m
	}
	return ZenModel{
		ID:          modelID,
		DisplayName: modelID,
		Model:       modelID,
		Multiplier:  1,
		ProviderID:  "openai",
	}
}

// ValidationError records a single catalog inconsistency found during startup.
type ValidationError struct {
	ModelID string
	Issue   string
}

// ValidateCatalog checks the built-in model catalog for obvious configuration
// mistakes. It returns every issue found so callers can decide whether to log
// or abort.
func ValidateCatalog() []ValidationError {
	var errs []ValidationError
	seenID := map[string]string{}      // ID -> first occurrence provider
	seenGateway := map[string]string{} // gateway ID -> model ID

	for id, m := range ZenModels {
		if id == "" {
			errs = append(errs, ValidationError{Issue: "empty model key in ZenModels map"})
			continue
		}
		if m.ID == "" {
			errs = append(errs, ValidationError{ModelID: id, Issue: "ID is empty"})
		}
		if id != m.ID {
			errs = append(errs, ValidationError{ModelID: id, Issue: "map key does not match model.ID"})
		}
		if m.DisplayName == "" {
			errs = append(errs, ValidationError{ModelID: id, Issue: "DisplayName is empty"})
		}
		if m.ProviderID == "" {
			errs = append(errs, ValidationError{ModelID: id, Issue: "ProviderID is empty"})
		}

		// Duplicate ID check
		if prev, exists := seenID[m.ID]; exists {
			errs = append(errs, ValidationError{ModelID: m.ID, Issue: "duplicate ID (also used by " + prev + ")"})
		} else {
			seenID[m.ID] = m.ProviderID
		}

		// Duplicate GatewayID check (only when GatewayID is set)
		if m.GatewayID != "" {
			if prev, exists := seenGateway[m.GatewayID]; exists {
				errs = append(errs, ValidationError{ModelID: m.ID, Issue: "duplicate GatewayID " + m.GatewayID + " (also used by " + prev + ")"})
			} else {
				seenGateway[m.GatewayID] = m.ID
			}
		}
	}

	return errs
}
