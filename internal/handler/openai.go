package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"zencoder-2api/internal/model"
	"zencoder-2api/internal/service"
)

type OpenAIHandler struct {
	svc       *service.OpenAIService
	grokSvc   *service.GrokService
	geminiSvc *service.GeminiService
}

func NewOpenAIHandler() *OpenAIHandler {
	return &OpenAIHandler{
		svc:       service.NewOpenAIService(),
		grokSvc:   service.NewGrokService(),
		geminiSvc: service.NewGeminiService(),
	}
}

// Models returns the canonical model IDs supported by this proxy. Provider
// catalog model names may still be accepted by request handlers, but they are
// intentionally not exposed as legacy aliases here.
func (h *OpenAIHandler) Models(c *gin.Context) {
	items := make([]gin.H, 0)
	for _, zenModel := range model.ListZenModels() {
		items = append(items, gin.H{
			"id":       zenModel.ID,
			"object":   "model",
			"created":  int64(0),
			"owned_by": "zencoder",
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   items,
	})
}

// Model returns one model using the same public shape as the list endpoint.
func (h *OpenAIHandler) Model(c *gin.Context) {
	modelID := strings.TrimSpace(c.Param("model"))
	zenModel, ok := model.GetZenModel(modelID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{
			"message": "model not found",
			"type":    "invalid_request_error",
			"code":    "model_not_found",
		}})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":       zenModel.ID,
		"object":   "model",
		"created":  int64(0),
		"owned_by": "zencoder",
	})
}

// ChatCompletions 处理 POST /v1/chat/completions
func (h *OpenAIHandler) ChatCompletions(c *gin.Context) {
	body, err := readRequestBody(c)
	if err != nil {
		c.JSON(bodyErrorStatus(err), gin.H{"error": err.Error()})
		return
	}

	// 解析模型名以确定使用哪个服务
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": "model is required",
			"type":    "invalid_request_error",
		}})
		return
	}

	// 根据模型的 ProviderID 分流
	zenModel, known := model.GetZenModel(req.Model)
	if !known {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": "unknown model",
			"type":    "invalid_request_error",
		}})
		return
	}
	if zenModel.ProviderID == "gemini" || strings.HasPrefix(strings.ToLower(req.Model), "gemini-") {
		if err := h.geminiSvc.ChatCompletionsProxy(c.Request.Context(), c.Writer, body); err != nil && !c.Writer.Written() {
			h.handleError(c, err)
		}
		return
	}
	if zenModel.ProviderID == "xai" || strings.HasPrefix(strings.ToLower(req.Model), "grok-") {
		// Grok 模型使用 xAI 服务
		if err := h.grokSvc.ChatCompletionsProxy(c.Request.Context(), c.Writer, body); err != nil && !c.Writer.Written() {
			h.handleError(c, err)
		}
		return
	}

	// 其他模型使用 OpenAI 服务
	if err := h.svc.ChatCompletionsProxy(c.Request.Context(), c.Writer, body); err != nil && !c.Writer.Written() {
		h.handleError(c, err)
	}
}

// Responses 处理 POST /v1/responses
func (h *OpenAIHandler) Responses(c *gin.Context) {
	body, err := readRequestBody(c)
	if err != nil {
		c.JSON(bodyErrorStatus(err), gin.H{"error": err.Error()})
		return
	}

	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": "model is required",
			"type":    "invalid_request_error",
		}})
		return
	}

	// Gemini models are served through the native generateContent endpoint.
	// Adapt Responses requests before they reach the OpenAI Responses gateway.
	zenModel, known := model.GetZenModel(req.Model)
	if !known {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": "unknown model",
			"type":    "invalid_request_error",
		}})
		return
	}
	if zenModel.ProviderID == "gemini" || strings.HasPrefix(strings.ToLower(req.Model), "gemini-") {
		if err := h.geminiSvc.ResponsesProxy(c.Request.Context(), c.Writer, body); err != nil && !c.Writer.Written() {
			h.handleError(c, err)
		}
		return
	}
	if err := h.svc.ResponsesProxy(c.Request.Context(), c.Writer, body); err != nil && !c.Writer.Written() {
		h.handleError(c, err)
	}
}

// handleError 统一处理错误，特别是没有可用账号的错误
func (h *OpenAIHandler) handleError(c *gin.Context, err error) {
	var upstreamErr *service.UpstreamError
	if errors.As(err, &upstreamErr) {
		status := upstreamErr.Status()
		if status < http.StatusBadRequest || status > 599 {
			status = http.StatusBadGateway
		}
		body := upstreamErr.Body
		if len(body) == 0 {
			body = []byte(`{"error":{"message":"upstream request failed","type":"upstream_error"}}`)
		}
		c.Data(status, "application/json", body)
		return
	}

	if errors.Is(err, service.ErrNoAvailableAccount) {
		traceID := requestTraceID(c)
		errMsg := fmt.Sprintf("没有可用token（traceid: %s）", traceID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsg})
		return
	}
	traceID := requestTraceID(c)
	c.Header("X-Request-ID", traceID)
	c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
		"message":  "upstream request failed",
		"type":     "upstream_error",
		"trace_id": traceID,
	}})
}
