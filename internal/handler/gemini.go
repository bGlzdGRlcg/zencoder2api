package handler

import (
	"errors"
	"fmt"
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

// GetModel implements Gemini's single-model discovery endpoint.
func (h *GeminiHandler) GetModel(c *gin.Context) {
	modelName := strings.TrimPrefix(strings.TrimSpace(c.Param("model")), "models/")
	zenModel, ok := model.GetZenModel(modelName)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{
			"message": "model not found",
			"status":  "NOT_FOUND",
		}})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"name":                       "models/" + zenModel.ID,
		"baseModelId":                zenModel.ID,
		"version":                    "preview",
		"displayName":                zenModel.DisplayName,
		"description":                zenModel.DisplayName,
		"supportedGenerationMethods": []string{"generateContent", "streamGenerateContent"},
	})
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
	zenModel, known := model.GetZenModel(modelName)
	if !known {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "unknown Gemini model", "type": "invalid_request_error"}})
		return
	}
	action := parts[1]

	body, err := readRequestBody(c)
	if err != nil {
		c.JSON(bodyErrorStatus(err), gin.H{"error": err.Error()})
		return
	}

	switch action {
	case "generateContent":
		var err error
		if zenModel.ProviderID == "gemini" {
			err = h.svc.GenerateContentProxy(c.Request.Context(), c.Writer, modelName, body)
		} else {
			err = h.svc.CompatibleGenerateContentProxy(c.Request.Context(), c.Writer, modelName, body, false)
		}
		if err != nil && !c.Writer.Written() {
			h.handleError(c, err)
		}
	case "streamGenerateContent":
		var err error
		if zenModel.ProviderID == "gemini" {
			err = h.svc.StreamGenerateContentProxy(c.Request.Context(), c.Writer, modelName, body)
		} else {
			err = h.svc.CompatibleGenerateContentProxy(c.Request.Context(), c.Writer, modelName, body, true)
		}
		if err != nil && !c.Writer.Written() {
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
		traceID := requestTraceID(c)
		errMsg := fmt.Sprintf("没有可用token（traceid: %s）", traceID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsg})
		return
	}
	traceID := requestTraceID(c)
	c.Header("X-Request-ID", traceID)
	c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
		"message": "upstream request failed", "status": "INTERNAL", "trace_id": traceID,
	}})
}
