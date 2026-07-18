package service

import (
	"net/http"
	"runtime"

	"zencoder-2api/internal/model"

	"github.com/google/uuid"
)

const zencoderVersion = "3.68.0"

// SetZencoderHeaders 设置Zencoder自定义请求头
func SetZencoderHeaders(req *http.Request, account *model.Account, zenModel model.ZenModel) error {
	// Match the current CLI gateway request shape. The operation ID must be
	// present under both names: the CLI uses one for auth and one for provider
	// metadata.
	operationID := uuid.New().String()
	req.Header.Set("User-Agent", "zen-cli/"+zencoderVersion)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("zen-operation-id", operationID)

	// OAuth accounts use the same Bearer authentication as the VSCode extension.
	if err := ApplyZencoderAuth(req.Context(), req, account); err != nil {
		return err
	}

	// x-stainless 系列
	req.Header.Set("x-stainless-arch", "x64")
	req.Header.Set("x-stainless-lang", "js")
	req.Header.Set("x-stainless-os", runtime.GOOS)
	req.Header.Set("x-stainless-package-version", "0.70.1")
	req.Header.Set("x-stainless-retry-count", "0")
	req.Header.Set("x-stainless-runtime", "node")
	req.Header.Set("x-stainless-runtime-version", "go")

	// zen/zencoder 系列 - 使用随机版本和唯一 ID
	gatewayModelID := zenModel.ID
	if zenModel.GatewayID != "" {
		gatewayModelID = zenModel.GatewayID
	}
	req.Header.Set("zen-model-id", gatewayModelID)
	req.Header.Set("zencoder-arch", "x64")
	req.Header.Set("zencoder-auto-model", "false")
	req.Header.Set("zencoder-client-type", "vscode")
	req.Header.Set("zencoder-is-subagent", "false")
	req.Header.Set("zencoder-operation-id", operationID)
	req.Header.Set("zencoder-operation-type", "agent_call")
	req.Header.Set("zencoder-os", runtime.GOOS)
	req.Header.Set("zencoder-version", zencoderVersion)
	return nil
}
