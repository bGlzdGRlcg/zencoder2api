package handler

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"zencoder-2api/internal/model"
	"zencoder-2api/internal/service"
)

type GeminiHandler struct {
	svc *service.GeminiService
}

func NewGeminiHandler() *GeminiHandler {
	return &GeminiHandler{svc: service.NewGeminiService()}
}

// ListModels implements the native Gemini model discovery endpoint.
func (h *GeminiHandler) ListModels(c *gin.Context) {
	items := make([]gin.H, 0)
	for _, zenModel := range model.ListZenModels() {
		if zenModel.ProviderID != "gemini" {
			continue
		}
		items = append(items, gin.H{
			"name":                       "models/" + zenModel.ID,
			"baseModelId":                zenModel.ID,
			"version":                    "preview",
			"displayName":                zenModel.DisplayName,
			"description":                zenModel.DisplayName,
			"supportedGenerationMethods": []string{"generateContent", "streamGenerateContent"},
		})
	}
	c.JSON(http.StatusOK, gin.H{"models": items})
}

// generateTraceID 生成一个随机的 trace ID
func generateGeminiTraceID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// HandleRequest 处理 POST /v1beta/models/*path
// 路径格式: /model:action 例如 /gemini-3-flash-preview:streamGenerateContent
func (h *GeminiHandler) HandleRequest(c *gin.Context) {
	path := c.Param("path")
	// 去掉开头的斜杠
	path = strings.TrimPrefix(path, "/")

	// 解析 model:action
	parts := strings.SplitN(path, ":", 2)
	if len(parts) != 2 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path format"})
		return
	}

	modelName := parts[0]
	// The native Gemini list endpoint returns names such as
	// "models/gemini-3-flash-preview". Some SDKs pass that full resource name
	// back in the request path, while others pass only the model ID.
	modelName = strings.TrimPrefix(modelName, "models/")
	action := parts[1]

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	switch action {
	case "generateContent":
		if err := h.svc.GenerateContentProxy(c.Request.Context(), c.Writer, modelName, body); err != nil {
			h.handleError(c, err)
		}
	case "streamGenerateContent":
		if err := h.svc.StreamGenerateContentProxy(c.Request.Context(), c.Writer, modelName, body); err != nil {
			h.handleError(c, err)
		}
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported action: " + action})
	}
}

// handleError 统一处理错误，特别是没有可用账号的错误
func (h *GeminiHandler) handleError(c *gin.Context, err error) {
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
		traceID := generateGeminiTraceID()
		errMsg := fmt.Sprintf("没有可用token（traceid: %s）", traceID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsg})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}
