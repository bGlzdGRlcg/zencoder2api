package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"zencoder-2api/internal/database"
	"zencoder-2api/internal/model"
	"zencoder-2api/internal/secret"
	"zencoder-2api/internal/service"
)

func TestRefreshCreditsValidatesSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{
		`{}`,
		`{"ids":[1],"refresh_all":true}`,
		`{"ids":[0]}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/accounts/credits/refresh", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = request
		NewAccountHandler().RefreshCredits(context)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, want 400", body, recorder.Code)
		}
	}
}

func TestRefreshCreditsQueriesTokensWithoutOperation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initHandlerTestDatabase(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/quotas/me/tokens" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"remaining":17,"totalConsumedByUser":3,"totalUserBudget":20}`))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	token, err := secret.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "credits-no-operation", CredentialType: model.CredentialOAuth,
		AccessToken: token, TokenExpiresAt: time.Now().Add(time.Hour), CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/accounts/credits/refresh", bytes.NewBufferString(`{"ids":[`+jsonUint(account.ID)+`]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	NewAccountHandler().RefreshCredits(context)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Items     []creditsRefreshItem `json:"items"`
		Requested int                  `json:"requested"`
		Refreshed int                  `json:"refreshed"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Requested != 1 || payload.Refreshed != 1 || len(payload.Items) != 1 ||
		payload.Items[0].UsageBasedCredits.State != service.UsageCreditsStateReady ||
		payload.Items[0].UsageBasedCredits.Remaining != 17 {
		t.Fatalf("unexpected response: %s", recorder.Body.String())
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.UsageCreditsStatus != service.UsageCreditsStateReady || stored.UsageCreditsLastAttemptAt == nil || stored.UsageCreditsRemaining != 17 {
		t.Fatalf("token balance was not persisted: %#v", stored)
	}
}

func TestAccountListNestsUsageCreditsWithoutInternalFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initHandlerTestDatabase(t)
	token, err := secret.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	account := model.Account{
		ClientID: "credits-list", CredentialType: model.CredentialOAuth,
		AccessToken: token, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
		UsageCreditsOperationCredits: 8, UsageCreditsTurns: 1, UsageCreditsOperationExists: true,
		UsageCreditsConsumed: 8, UsageCreditsBudget: 5000, UsageCreditsRemaining: 4992,
		UsageCreditsAvailable: true, UsageCreditsStatus: service.UsageCreditsStateReady,
		UsageCreditsUpdatedAt: &now, UsageCreditsCredentialRevision: 1,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/accounts?page=1&size=10", nil)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	NewAccountHandler().List(context)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	items := payload["items"].([]interface{})
	item := items[0].(map[string]interface{})
	credits := item["usage_based_credits"].(map[string]interface{})
	if credits["remaining"] != float64(4992) || credits["state"] != service.UsageCreditsStateReady {
		t.Fatalf("unexpected nested credits: %#v", credits)
	}
	for _, internal := range []string{"usage_credits_operation_id", "usage_credits_lease_id", "usage_credits_status"} {
		if _, exposed := item[internal]; exposed {
			t.Fatalf("internal field %q was exposed", internal)
		}
	}
}

func jsonUint(value uint) string {
	data, _ := json.Marshal(value)
	return string(data)
}
