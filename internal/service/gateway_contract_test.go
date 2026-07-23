package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"zencoder-2api/internal/database"
	"zencoder-2api/internal/model"
	"zencoder-2api/internal/secret"
)

func TestGatewayProviderContracts(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("g", 32))))
	t.Setenv("ZENCODER_CLIENT_TYPE", "zencoder2api-test")
	t.Setenv("ZENCODER_CLIENT_VERSION", "test")

	expected := map[string]int{
		"/openai/v1/chat/completions":                                        1,
		"/openai/v1/responses":                                               1,
		"/anthropic/v1/messages":                                             1,
		"/gemini/v1beta/models/gemini-3-flash-preview:generateContent":       1,
		"/gemini/v1beta/models/gemini-3-flash-preview:streamGenerateContent": 1,
		"/xai/v1/chat/completions":                                           1,
	}
	seen := make(map[string]int)
	var seenMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer access" {
			t.Errorf("missing OAuth bearer header: %#v", r.Header)
		}
		if r.Header.Get("zen-operation-id") == "" || r.Header.Get("zen-operation-id") != r.Header.Get("zencoder-operation-id") {
			t.Errorf("invalid operation metadata: %#v", r.Header)
		}
		if r.Header.Get("zencoder-client-type") != "zencoder2api-test" || r.Header.Get("x-stainless-lang") != "" {
			t.Errorf("invalid client metadata: %#v", r.Header)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		if body["model"] == "" && !strings.HasPrefix(r.URL.Path, "/gemini/") {
			t.Errorf("missing provider model in body for %s", r.URL.Path)
		}
		switch r.URL.Path {
		case "/anthropic/v1/messages":
			if body["model"] != "claude-haiku-4-5-20251001" || r.Header.Get("zen-model-id") != "haiku-4-5-think" {
				t.Errorf("Anthropic public/Gateway/provider model split was lost: model=%v zen-model-id=%q", body["model"], r.Header.Get("zen-model-id"))
			}
		case "/gemini/v1beta/models/gemini-3-flash-preview:streamGenerateContent":
			if r.URL.Query().Get("alt") != "sse" {
				t.Errorf("Gemini stream request missing alt=sse: %s", r.URL.RawQuery)
			}
		}
		seenMu.Lock()
		seen[r.URL.Path]++
		seenMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)

	if err := database.Init(filepath.Join(t.TempDir(), "gateway.db")); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	encrypted, err := secret.Encrypt("access")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "gateway-contract", CredentialType: model.CredentialOAuth,
		AccessToken: encrypted, TokenExpiresAt: time.Now().Add(time.Hour),
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	if err := pool.refresh(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	openAI := NewOpenAIService()
	anthropic := NewAnthropicService()
	gemini := NewGeminiService()
	grok := NewGrokService()
	calls := []func() (*http.Response, error){
		func() (*http.Response, error) {
			return openAI.ChatCompletions(ctx, []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"test"}]}`))
		},
		func() (*http.Response, error) {
			return openAI.Responses(ctx, []byte(`{"model":"gpt-5.4","input":"test"}`))
		},
		func() (*http.Response, error) {
			return anthropic.Messages(ctx, []byte(`{"model":"claude-haiku-4-5","max_tokens":8192,"messages":[{"role":"user","content":"test"}]}`), false)
		},
		func() (*http.Response, error) {
			return gemini.GenerateContent(ctx, "gemini-3-flash-preview", []byte(`{"contents":[{"role":"user","parts":[{"text":"test"}]}]}`))
		},
		func() (*http.Response, error) {
			return gemini.StreamGenerateContent(ctx, "gemini-3-flash-preview", []byte(`{"contents":[{"role":"user","parts":[{"text":"test"}]}]}`))
		},
		func() (*http.Response, error) {
			return grok.ChatCompletions(ctx, []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"test"}]}`))
		},
	}
	for _, call := range calls {
		response, err := call()
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	for path, count := range expected {
		if seen[path] != count {
			t.Errorf("path %s called %d times, want %d; all=%#v", path, seen[path], count, seen)
		}
	}
}

func TestOpenAIFailoverUsesInitialAccountCount(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("f", 32))))
	var seenMu sync.Mutex
	seen := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("zencoder-api-key")
		seenMu.Lock()
		seen = append(seen, key)
		seenMu.Unlock()
		if key == "first-key" {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	if err := database.Init(filepath.Join(t.TempDir(), "failover.db")); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	for index, key := range []string{"first-key", "second-key"} {
		encrypted, err := secret.Encrypt(key)
		if err != nil {
			t.Fatal(err)
		}
		account := &model.Account{
			ClientID: "failover-" + key, CredentialType: model.CredentialAPIKey,
			APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
			ID: uint(index + 1),
		}
		if err := database.GetDB().Create(account).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.refresh(); err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	pool.index = 0
	pool.mu.Unlock()
	response, err := NewOpenAIService().ChatCompletions(context.Background(), []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"test"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	seenMu.Lock()
	defer seenMu.Unlock()
	if len(seen) != 2 || seen[0] != "first-key" || seen[1] != "second-key" {
		t.Fatalf("failover attempts = %#v", seen)
	}
}

func TestAnthropicFailoverRetriesGenericServerError(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32))))
	var seenMu sync.Mutex
	seen := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("zencoder-api-key")
		seenMu.Lock()
		seen = append(seen, key)
		seenMu.Unlock()
		if key == "anthropic-first-key" {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_test","type":"message","content":[],"stop_reason":"end_turn"}`))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	if err := database.Init(filepath.Join(t.TempDir(), "anthropic-failover.db")); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	for index, key := range []string{"anthropic-first-key", "anthropic-second-key"} {
		encrypted, err := secret.Encrypt(key)
		if err != nil {
			t.Fatal(err)
		}
		account := &model.Account{
			ClientID: "anthropic-failover-" + key, CredentialType: model.CredentialAPIKey,
			APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
			ID: uint(index + 1),
		}
		if err := database.GetDB().Create(account).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.refresh(); err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	pool.index = 0
	pool.mu.Unlock()
	response, err := NewAnthropicService().Messages(context.Background(), []byte(`{"model":"claude-sonnet-4-6","max_tokens":64,"messages":[{"role":"user","content":"test"}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	seenMu.Lock()
	defer seenMu.Unlock()
	if len(seen) != 2 || seen[0] != "anthropic-first-key" || seen[1] != "anthropic-second-key" {
		t.Fatalf("Anthropic failover attempts = %#v", seen)
	}
}
