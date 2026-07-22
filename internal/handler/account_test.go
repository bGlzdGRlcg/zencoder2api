package handler

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"zencoder-2api/internal/database"
	"zencoder-2api/internal/model"
	"zencoder-2api/internal/secret"
)

func initHandlerTestDatabase(t *testing.T) {
	t.Helper()
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("h", 32))))
	if err := database.Init(filepath.Join(t.TempDir(), "handler.db")); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
}

func TestAccountListRejectsUnboundedPageSize(t *testing.T) {
	gin.SetMode(gin.TestMode)
	request := httptest.NewRequest(http.MethodGet, "/api/accounts?page=1&size=101", nil)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	NewAccountHandler().List(context)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAccountBatchDeleteAll(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initHandlerTestDatabase(t)
	token, err := secret.Encrypt("access")
	if err != nil {
		t.Fatal(err)
	}
	for _, clientID := range []string{"one", "two"} {
		if err := database.GetDB().Create(&model.Account{
			ClientID: clientID, CredentialType: model.CredentialOAuth, AccessToken: token,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	request := httptest.NewRequest(http.MethodPost, "/api/accounts/batch/delete", bytes.NewBufferString(`{"delete_all":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	NewAccountHandler().BatchDelete(context)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	var count int64
	if err := database.GetDB().Model(&model.Account{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("delete_all left %d accounts", count)
	}
}

func TestAPIKeyCreateAndRotateStayEncrypted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initHandlerTestDatabase(t)

	const initialKey = "fixture-api-key-1234"
	const rotatedKey = "rotated-api-key-5678"
	create := httptest.NewRequest(http.MethodPost, "/api/accounts/api-key", bytes.NewBufferString(`{"api_key":"`+initialKey+`","name":"test"}`))
	create.Header.Set("Content-Type", "application/json")
	createRecorder := httptest.NewRecorder()
	createContext, _ := gin.CreateTestContext(createRecorder)
	createContext.Request = create
	NewAccountHandler().CreateAPIKey(createContext)
	if createRecorder.Code != http.StatusCreated {
		t.Fatalf("unexpected create status: %d body=%s", createRecorder.Code, createRecorder.Body.String())
	}
	if strings.Contains(createRecorder.Body.String(), initialKey) {
		t.Fatal("API key leaked in create response")
	}
	var account model.Account
	if err := database.GetDB().Where("credential_type = ?", model.CredentialAPIKey).First(&account).Error; err != nil {
		t.Fatal(err)
	}
	if !secret.IsEncrypted(account.APIKey) {
		t.Fatalf("API key was not encrypted at rest: %q", account.APIKey)
	}
	if err := database.GetDB().Model(&account).Updates(map[string]interface{}{
		"usage_credits_query_revision": 20,
		"usage_credits_available":      true,
		"usage_credits_status":         "ready",
		"usage_credits_remaining":      4992,
		"usage_credits_operation_id":   "old-operation",
		"usage_credits_lease_id":       "old-holder",
	}).Error; err != nil {
		t.Fatal(err)
	}

	rotate := httptest.NewRequest(http.MethodPut, "/api/accounts/1/api-key", bytes.NewBufferString(`{"api_key":"`+rotatedKey+`"}`))
	rotate.Header.Set("Content-Type", "application/json")
	rotateRecorder := httptest.NewRecorder()
	rotateContext, _ := gin.CreateTestContext(rotateRecorder)
	rotateContext.Request = rotate
	rotateContext.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(uint64(account.ID), 10)}}
	NewAccountHandler().RotateAPIKey(rotateContext)
	if rotateRecorder.Code != http.StatusOK {
		t.Fatalf("unexpected rotate status: %d body=%s", rotateRecorder.Code, rotateRecorder.Body.String())
	}
	if err := database.GetDB().First(&account, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	plaintext, err := secret.Decrypt(account.APIKey)
	if err != nil {
		t.Fatal(err)
	}
	if plaintext != rotatedKey || account.AccessToken != "" || account.RefreshToken != "" {
		t.Fatal("API-key rotation did not preserve credential exclusivity")
	}
	wantClientID, err := apiKeyClientID(rotatedKey)
	if err != nil {
		t.Fatal(err)
	}
	if account.ClientID != wantClientID || strings.Contains(account.ClientID, rotatedKey) {
		t.Fatalf("rotated API-key identity was not updated: %q", account.ClientID)
	}
	if account.CredentialRevision != 2 || account.UsageCreditsQueryRevision != 21 {
		t.Fatalf("rotation revisions = credential:%d credits:%d, want 2 and 21", account.CredentialRevision, account.UsageCreditsQueryRevision)
	}
	if account.UsageCreditsAvailable || account.UsageCreditsStatus != "unknown" || account.UsageCreditsRemaining != 0 ||
		account.UsageCreditsOperationID != "" || account.UsageCreditsLeaseID != "" {
		t.Fatalf("rotation retained the previous credential's credit snapshot: %+v", account)
	}

	recreate := httptest.NewRequest(http.MethodPost, "/api/accounts/api-key", bytes.NewBufferString(`{"api_key":"`+rotatedKey+`"}`))
	recreate.Header.Set("Content-Type", "application/json")
	recreateRecorder := httptest.NewRecorder()
	recreateContext, _ := gin.CreateTestContext(recreateRecorder)
	recreateContext.Request = recreate
	NewAccountHandler().CreateAPIKey(recreateContext)
	if recreateRecorder.Code != http.StatusCreated {
		t.Fatalf("unexpected recreate status: %d body=%s", recreateRecorder.Code, recreateRecorder.Body.String())
	}
	var count int64
	if err := database.GetDB().Model(&model.Account{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("re-adding a rotated API key created %d accounts", count)
	}
}

func TestAPIKeyCreateRejectsLowEntropyCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initHandlerTestDatabase(t)
	request := httptest.NewRequest(http.MethodPost, "/api/accounts/api-key", bytes.NewBufferString(`{"api_key":"short"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	NewAccountHandler().CreateAPIKey(context)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestConcurrentAPIKeyCreateAndRotateKeepIdentityConsistent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initHandlerTestDatabase(t)
	handler := NewAccountHandler()

	for index := 0; index < 8; index++ {
		initialKey := fmt.Sprintf("concurrent-initial-key-%02d", index)
		rotatedKey := fmt.Sprintf("concurrent-rotated-key-%02d", index)
		initialRecorder := invokeCreateAPIKey(t, handler, initialKey)
		if initialRecorder.Code != http.StatusCreated {
			t.Fatalf("initial create status = %d body=%s", initialRecorder.Code, initialRecorder.Body.String())
		}
		initialID, err := apiKeyClientID(initialKey)
		if err != nil {
			t.Fatal(err)
		}
		var account model.Account
		if err := database.GetDB().Where("client_id = ?", initialID).First(&account).Error; err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		var createRecorder, rotateRecorder *httptest.ResponseRecorder
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			createRecorder = invokeCreateAPIKey(t, handler, initialKey)
		}()
		go func() {
			defer wg.Done()
			<-start
			rotateRecorder = invokeRotateAPIKey(t, handler, account.ID, rotatedKey)
		}()
		close(start)
		wg.Wait()
		if createRecorder.Code != http.StatusCreated {
			t.Fatalf("concurrent create status = %d body=%s", createRecorder.Code, createRecorder.Body.String())
		}
		if rotateRecorder.Code != http.StatusOK {
			t.Fatalf("concurrent rotate status = %d body=%s", rotateRecorder.Code, rotateRecorder.Body.String())
		}
	}

	var accounts []model.Account
	if err := database.GetDB().Where("credential_type = ?", model.CredentialAPIKey).Find(&accounts).Error; err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		plaintext, err := secret.Decrypt(account.APIKey)
		if err != nil {
			t.Fatal(err)
		}
		wantClientID, err := apiKeyClientID(plaintext)
		if err != nil {
			t.Fatal(err)
		}
		if account.ClientID != wantClientID {
			t.Fatalf("API-key identity mismatch: account=%d client_id=%q want=%q", account.ID, account.ClientID, wantClientID)
		}
	}
}

func invokeCreateAPIKey(t *testing.T, handler *AccountHandler, key string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/accounts/api-key", bytes.NewBufferString(`{"api_key":"`+key+`"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	handler.CreateAPIKey(context)
	return recorder
}

func invokeRotateAPIKey(t *testing.T, handler *AccountHandler, id uint, key string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPut, "/api/accounts/"+strconv.FormatUint(uint64(id), 10)+"/api-key", bytes.NewBufferString(`{"api_key":"`+key+`"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	context.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(uint64(id), 10)}}
	handler.RotateAPIKey(context)
	return recorder
}
