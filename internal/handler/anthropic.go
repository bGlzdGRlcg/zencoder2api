package handler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

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

// generateTraceID 生成一个随机的 trace ID
func generateAnthropicTraceID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Messages 处理 POST /v1/messages
func (h *AnthropicHandler) Messages(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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

	// Cherry can use the Anthropic protocol for an OpenAI model. Adapt that
	// request at the proxy boundary instead of forwarding Anthropic thinking
	// parameters to the OpenAI gateway.
	providerID := model.ResolveOpenAIModel(req.Model).ProviderID
	if providerID == "openai" {
		if err := h.openAISvc.MessagesProxy(ctx, c.Writer, body); err != nil {
			h.handleError(c, err)
		}
		return
	}
	if providerID == "gemini" {
		if err := h.geminiSvc.MessagesProxy(ctx, c.Writer, body); err != nil {
			h.handleError(c, err)
		}
		return
	}

	if err := h.svc.MessagesProxy(ctx, c.Writer, body); err != nil {
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
		traceID := generateAnthropicTraceID()
		errMsg := fmt.Sprintf("没有可用token（traceid: %s）", traceID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsg})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}
