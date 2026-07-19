package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zencoder-2api/internal/model"
)

func TestOperationIDStableAcrossHeaderBuilds(t *testing.T) {
	ctx := ensureOperationID(context.Background())
	account := &model.Account{
		AccessToken:    "test-token",
		CredentialType: model.CredentialOAuth,
		TokenExpiresAt: time.Now().Add(time.Hour),
	}
	zenModel, ok := model.GetZenModel("gpt-5.4")
	if !ok {
		t.Fatal("test model missing")
	}
	one, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", nil)
	two, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", nil)
	if err := SetZencoderHeaders(one, account, zenModel); err != nil {
		t.Fatal(err)
	}
	if err := SetZencoderHeaders(two, account, zenModel); err != nil {
		t.Fatal(err)
	}
	if one.Header.Get("zencoder-operation-id") == "" || one.Header.Get("zencoder-operation-id") != two.Header.Get("zencoder-operation-id") {
		t.Fatalf("operation ID was not stable: %q / %q", one.Header.Get("zencoder-operation-id"), two.Header.Get("zencoder-operation-id"))
	}
	if got := one.Header.Get("x-stainless-runtime-version"); got != "" {
		t.Fatalf("unexpected fabricated SDK header: %q", got)
	}
}

func TestModelExtraHeadersCannotOverrideGatewayIdentity(t *testing.T) {
	ctx := ensureOperationID(context.Background())
	account := &model.Account{
		AccessToken:    "test-token",
		CredentialType: model.CredentialOAuth,
		TokenExpiresAt: time.Now().Add(time.Hour),
	}
	zenModel := model.ZenModel{
		ID:        "public-model",
		GatewayID: "gateway-model",
		Parameters: &model.ModelParameters{ExtraHeaders: map[string]string{
			"Anthropic-Beta":          "allowed-feature",
			"Zen-Model-ID":            "attacker-model",
			"Zencoder-Client-Type":    "attacker-client",
			"Zencoder-Operation-Type": "attacker-operation",
			"Zencoder-Version":        "attacker-version",
			"Zencoder-OS":             "attacker-os",
			"Zencoder-Arch":           "attacker-arch",
			"Zencoder-Auto-Model":     "true",
			"Zencoder-Is-Subagent":    "true",
			"Zencoder-Agent":          "attacker-agent",
		}},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetZencoderHeaders(req, account, zenModel); err != nil {
		t.Fatal(err)
	}
	ApplyModelExtraHeaders(req, zenModel)

	if got := req.Header.Get("Zen-Model-ID"); got != "gateway-model" {
		t.Fatalf("zen-model-id was overridden: %q", got)
	}
	for _, key := range []string{
		"Zencoder-Client-Type", "Zencoder-Operation-Type", "Zencoder-Version",
		"Zencoder-OS", "Zencoder-Arch", "Zencoder-Auto-Model", "Zencoder-Is-Subagent",
	} {
		if got := req.Header.Get(key); strings.HasPrefix(got, "attacker") || got == "true" {
			t.Fatalf("%s was overridden: %q", key, got)
		}
	}
	if got := req.Header.Get("Anthropic-Beta"); got != "allowed-feature" {
		t.Fatalf("safe model header was not applied: %q", got)
	}
}

func TestRetryableUpstreamStatusesExcludeRequestConflicts(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable, 529} {
		if !shouldRetryUpstreamStatus(status) {
			t.Errorf("status %d should be retryable", status)
		}
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusNotImplemented, http.StatusHTTPVersionNotSupported} {
		if shouldRetryUpstreamStatus(status) {
			t.Errorf("status %d should not be retryable", status)
		}
	}
}

func TestGatewayClientDoesNotFollowRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/other", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := newDirectHTTPClient(time.Second)
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("redirect was followed: status=%d", resp.StatusCode)
	}
}

func TestValidateUpstreamEndpointConfigRequiresTLS(t *testing.T) {
	for _, valid := range []string{"https://gateway.example.test/base", "http://localhost:8081", "http://127.0.0.1:8081"} {
		t.Setenv("ZENCODER_GATEWAY_BASE_URL", valid)
		t.Setenv("ZENCODER_AUTH_BASE_URL", "")
		if err := ValidateUpstreamEndpointConfig(); err != nil {
			t.Fatalf("valid endpoint %q rejected: %v", valid, err)
		}
	}
	for _, invalid := range []string{"http://gateway.example.test", "https://user:pass@gateway.example.test", "https://gateway.example.test?token=value"} {
		t.Setenv("ZENCODER_GATEWAY_BASE_URL", invalid)
		if err := ValidateUpstreamEndpointConfig(); err == nil {
			t.Fatalf("invalid endpoint %q accepted", invalid)
		}
	}
}

func TestResponseHeadersDropHopByHopAndSecrets(t *testing.T) {
	recorder := httptest.NewRecorder()
	upstreamHeaders := http.Header{
		"Connection":                           {"keep-alive, X-Internal"},
		"X-Internal":                           {"secret-hop"},
		"Set-Cookie":                           {"session=secret"},
		"Authorization":                        {"Bearer secret"},
		"Content-Type":                         {"application/json"},
		"Zen-Request-Cost":                     {"1"},
		"Content-Length":                       {"100"},
		"Content-Encoding":                     {"gzip"},
		"X-Request-Id":                         {"request"},
		"Access-Control-Allow-Origin":          {"*"},
		"Access-Control-Allow-Credentials":     {"true"},
		"Access-Control-Allow-Headers":         {"Authorization"},
		"Access-Control-Allow-Methods":         {"POST"},
		"Access-Control-Expose-Headers":        {"X-Secret"},
		"Access-Control-Max-Age":               {"3600"},
		"Access-Control-Allow-Private-Network": {"true"},
		"Strict-Transport-Security":            {"max-age=31536000"},
		"Content-Security-Policy":              {"default-src 'none'"},
		"Content-Security-Policy-Report-Only":  {"default-src 'none'"},
		"X-Content-Type-Options":               {"nosniff"},
		"X-Frame-Options":                      {"DENY"},
		"Referrer-Policy":                      {"no-referrer"},
		"Permissions-Policy":                   {"camera=()"},
		"Forwarded":                            {"for=192.0.2.1"},
		"X-Forwarded-For":                      {"192.0.2.1"},
		"X-Forwarded-Host":                     {"upstream.example"},
		"X-Forwarded-Proto":                    {"https"},
		"X-Forwarded-Port":                     {"443"},
		"X-Forwarded-Prefix":                   {"/internal"},
		"Server":                               {"upstream"},
		"Via":                                  {"1.1 upstream"},
		"Alt-Svc":                              {`h3=":443"`},
	}
	copyResponseHeaders(recorder.Header(), upstreamHeaders, true)

	for _, key := range []string{
		"Connection", "X-Internal", "Set-Cookie", "Authorization", "Content-Length", "Content-Encoding",
		"X-Request-Id", "Access-Control-Allow-Origin", "Access-Control-Allow-Credentials",
		"Access-Control-Allow-Headers", "Access-Control-Allow-Methods", "Access-Control-Expose-Headers",
		"Access-Control-Max-Age", "Access-Control-Allow-Private-Network", "Strict-Transport-Security",
		"Content-Security-Policy", "Content-Security-Policy-Report-Only", "X-Content-Type-Options",
		"X-Frame-Options", "Referrer-Policy", "Permissions-Policy", "Forwarded", "X-Forwarded-For",
		"X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port", "X-Forwarded-Prefix", "Server", "Via", "Alt-Svc",
	} {
		if recorder.Header().Get(key) != "" {
			t.Fatalf("sensitive/hop header copied: %s", key)
		}
	}
	if recorder.Header().Get("Content-Type") != "application/json" || recorder.Header().Get("Zen-Request-Cost") != "1" {
		t.Fatal("safe response metadata was not copied")
	}
}

func TestReadSSELineLimit(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader(strings.Repeat("x", maxSSELineBytes+1) + "\n"))
	if _, err := readSSELine(reader); err == nil {
		t.Fatal("expected oversized SSE line error")
	}
}

func TestUnknownModelReturnsClientError(t *testing.T) {
	_, err := NewOpenAIService().ChatCompletions(context.Background(), []byte(`{"model":"does-not-exist","messages":[{"role":"user","content":"hi"}]}`))
	var upstream *UpstreamError
	if !errors.As(err, &upstream) || upstream.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown model error = %v", err)
	}
}

func TestChatToAnthropicRejectsLossyFields(t *testing.T) {
	_, err := convertChatToAnthropicBody([]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stop":"END","n":2}`))
	if err == nil {
		t.Fatal("expected unsupported n to be rejected")
	}
	converted, err := convertChatToAnthropicBody([]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stop":"END"}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(converted, &body); err != nil {
		t.Fatal(err)
	}
	stops, ok := body["stop_sequences"].([]interface{})
	if !ok || len(stops) != 1 || stops[0] != "END" {
		t.Fatalf("stop_sequences = %#v", body["stop_sequences"])
	}
	if _, exists := body["n"]; exists {
		t.Fatal("unsupported Anthropic parameter n was forwarded")
	}
}
