package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"zencoder-2api/internal/model"
	"zencoder-2api/internal/service"

	"github.com/gin-gonic/gin"
)

type AnthropicHandler struct {
	svc       *service.AnthropicService
	openAISvc *service.OpenAIService
	geminiSvc *service.GeminiService
}

func NewAnthropicHandler() *AnthropicHandler {
	return &AnthropicHandler{
		svc:       service.NewAnthropicService(),
		openAISvc: service.NewOpenAIService(),
		geminiSvc: service.NewGeminiService(),
	}
}

// Messages 处理 POST /v1/messages
func (h *AnthropicHandler) Messages(c *gin.Context) {
	body, err := readRequestBody(c)
	if err != nil {
		c.JSON(bodyErrorStatus(err), gin.H{"error": err.Error()})
		return
	}

	// 传递原始请求头给service层，用于错误日志记录
	ctx := service.WithOriginalHeaders(c.Request.Context(), c.Request.Header)
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "model is required", "type": "invalid_request_error"}})
		return
	}

	// Cherry can use the Anthropic protocol for an OpenAI model. Adapt that
	// request at the proxy boundary instead of forwarding Anthropic thinking
	// parameters to the OpenAI gateway.
	zenModel, known := model.GetZenModel(req.Model)
	if !known {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "unknown model", "type": "invalid_request_error"}})
		return
	}
	providerID := zenModel.ProviderID
	if providerID == "openai" {
		if err := h.openAISvc.MessagesProxy(ctx, c.Writer, body); err != nil && !c.Writer.Written() {
			h.handleError(c, err)
		}
		return
	}
	if providerID == "xai" {
		if err := h.openAISvc.MessagesProxy(ctx, c.Writer, body); err != nil && !c.Writer.Written() {
			h.handleError(c, err)
		}
		return
	}
	if providerID == "gemini" {
		if err := h.geminiSvc.MessagesProxy(ctx, c.Writer, body); err != nil && !c.Writer.Written() {
			h.handleError(c, err)
		}
		return
	}

	if err := h.svc.MessagesProxy(ctx, c.Writer, body); err != nil && !c.Writer.Written() {
		h.handleError(c, err)
	}
}

// handleError 统一处理错误，特别是没有可用账号的错误
func (h *AnthropicHandler) handleError(c *gin.Context, err error) {
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
		"type": "api_error", "message": "upstream request failed", "trace_id": traceID,
	}})
}
