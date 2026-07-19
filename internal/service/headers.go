package service

import (
	"context"
	"net/http"
	"os"
	"runtime"
	"strings"

	"zencoder-2api/internal/model"

	"github.com/google/uuid"
)

const zencoderVersion = "2api"

type operationIDContextKey struct{}

func ensureOperationID(ctx context.Context) context.Context {
	if operationID, _ := ctx.Value(operationIDContextKey{}).(string); operationID != "" {
		return ctx
	}
	return context.WithValue(ctx, operationIDContextKey{}, uuid.New().String())
}

func operationIDFromContext(ctx context.Context) string {
	if operationID, _ := ctx.Value(operationIDContextKey{}).(string); operationID != "" {
		return operationID
	}
	return uuid.New().String()
}

func gatewayMetadata(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// ApplyModelExtraHeaders applies catalog headers without allowing a model
// definition to replace authentication, routing, or transport semantics.
func ApplyModelExtraHeaders(req *http.Request, zenModel model.ZenModel) {
	if zenModel.Parameters == nil {
		return
	}
	for key, value := range zenModel.Parameters.ExtraHeaders {
		normalizedKey := strings.ToLower(strings.TrimSpace(key))
		switch normalizedKey {
		case "authorization", "zencoder-api-key", "x-api-key", "x-goog-api-key",
			"host", "content-length", "content-type", "user-agent",
			"zen-model-id", "zen-operation-id", "zencoder-agent",
			"zencoder-arch", "zencoder-auto-model", "zencoder-client-type",
			"zencoder-is-subagent", "zencoder-operation-id",
			"zencoder-operation-type", "zencoder-os", "zencoder-version":
			continue
		default:
			req.Header.Set(key, value)
		}
	}
}

// SetZencoderHeaders 设置Zencoder自定义请求头
func SetZencoderHeaders(req *http.Request, account *model.Account, zenModel model.ZenModel) error {
	// Match the current CLI gateway request shape. The operation ID must be
	// present under both names: the CLI uses one for auth and one for provider
	// metadata.
	operationID := operationIDFromContext(req.Context())
	clientVersion := gatewayMetadata("ZENCODER_CLIENT_VERSION", zencoderVersion)
	req.Header.Set("User-Agent", "zen-cli/"+clientVersion)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("zen-operation-id", operationID)

	// OAuth accounts use the same Bearer authentication as the VSCode extension.
	if err := ApplyZencoderAuth(req.Context(), req, account); err != nil {
		return err
	}

	// zen/zencoder metadata mirrors the host identity without pretending this
	// Go client is a particular JavaScript SDK build.
	gatewayModelID := zenModel.ID
	if zenModel.GatewayID != "" {
		gatewayModelID = zenModel.GatewayID
	}
	req.Header.Set("zen-model-id", gatewayModelID)
	req.Header.Set("zencoder-arch", runtime.GOARCH)
	req.Header.Set("zencoder-auto-model", "false")
	req.Header.Set("zencoder-client-type", gatewayMetadata("ZENCODER_CLIENT_TYPE", "zencoder2api"))
	req.Header.Set("zencoder-is-subagent", gatewayMetadata("ZENCODER_IS_SUBAGENT", "false"))
	req.Header.Set("zencoder-operation-id", operationID)
	req.Header.Set("zencoder-operation-type", gatewayMetadata("ZENCODER_OPERATION_TYPE", "agent_call"))
	req.Header.Set("zencoder-os", runtime.GOOS)
	req.Header.Set("zencoder-version", clientVersion)
	if agent := strings.TrimSpace(os.Getenv("ZENCODER_AGENT")); agent != "" {
		req.Header.Set("zencoder-agent", agent)
	}
	return nil
}
