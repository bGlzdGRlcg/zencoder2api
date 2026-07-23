package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestOpenAIGetModelUsesPublicShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models/gpt-5.4", nil)
	ctx.Params = gin.Params{{Key: "model", Value: "gpt-5.4"}}
	NewOpenAIHandler().Model(ctx)
	if recorder.Code != http.StatusOK || recorder.Body.String() == "" {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != `{"created":0,"id":"gpt-5.4","object":"model","owned_by":"zencoder"}` {
		t.Fatalf("unexpected public model response: %s", body)
	}
}

func TestGeminiGetModelRejectsNonGemini(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models/gpt-5.4", nil)
	ctx.Params = gin.Params{{Key: "model", Value: "gpt-5.4"}}
	NewGeminiHandler().GetModel(ctx)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want %d", recorder.Code, http.StatusNotFound)
	}
}
