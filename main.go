package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"zencoder-2api/internal/database"
	"zencoder-2api/internal/handler"
	"zencoder-2api/internal/logging"
	"zencoder-2api/internal/middleware"
	"zencoder-2api/internal/model"
	"zencoder-2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/render"
)

//go:embed web/templates/* web/static/*
var embeddedWebFiles embed.FS

const (
	defaultPort            = 8080
	streamWriteIdleTimeout = 30 * time.Second
	shutdownTimeout        = 30 * time.Second
)

type serverConfig struct {
	address      string
	databasePath string
}

func main() {
	if err := run(); err != nil {
		logging.Fatalf("%v", err)
	}
}

func run() error {
	logging.Init()
	gin.SetMode(gin.ReleaseMode)

	cfg, err := loadServerConfig()
	if err != nil {
		return err
	}
	if err := service.ValidateUpstreamEndpointConfig(); err != nil {
		return err
	}

	// Catalog errors are programming errors. Serving a partially valid catalog
	// causes requests to be routed to the wrong provider, so fail at startup.
	if validationErrors := model.ValidateCatalog(); len(validationErrors) > 0 {
		var details strings.Builder
		for _, validationErr := range validationErrors {
			fmt.Fprintf(&details, "%s: %s; ", validationErr.ModelID, validationErr.Issue)
		}
		return fmt.Errorf("invalid model catalog: %s", strings.TrimSuffix(details.String(), "; "))
	}

	if err := database.Init(cfg.databasePath); err != nil {
		return fmt.Errorf("init database: %w", err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		return fmt.Errorf("get database connection: %w", err)
	}
	defer func() {
		if err := sqlDB.Close(); err != nil {
			logging.Warnf("Close database: %v", err)
		}
	}()

	// 启动积分重置定时任务，并在服务退出时停止它。
	stopCreditReset := service.StartCreditResetScheduler()
	defer stopCreditReset()

	// 初始化账号池
	service.InitAccountPool()
	defer service.StopAccountPool()
	stopUsageCreditsWorker := service.StartUsageCreditsWorker()
	defer stopUsageCreditsWorker()
	stopUsageCreditsScheduler := service.StartUsageCreditsRefreshScheduler()
	defer stopUsageCreditsScheduler()

	r := gin.New()
	r.Use(middleware.Recovery())
	r.Use(middleware.RequestID())
	r.Use(middleware.SecurityHeaders())
	r.Use(middleware.WriteIdleTimeout(streamWriteIdleTimeout))
	if err := r.SetTrustedProxies(nil); err != nil {
		return fmt.Errorf("configure trusted proxies: %w", err)
	}
	if err := setupRoutes(r); err != nil {
		return err
	}

	server := &http.Server{
		Addr:              cfg.address,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		// Streaming API responses have no fixed duration. The upstream clients
		// and graceful shutdown bound them; a server WriteTimeout would truncate
		// valid SSE responses.
		WriteTimeout:   0,
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 64 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logging.Infof("Listening on %s", cfg.address)
	return serve(ctx, server)
}

func loadServerConfig() (serverConfig, error) {
	bindAddress := strings.TrimSpace(os.Getenv("BIND_ADDRESS"))
	if bindAddress == "" {
		bindAddress = "0.0.0.0"
	}
	if !validBindAddress(bindAddress) {
		return serverConfig{}, fmt.Errorf("BIND_ADDRESS must be localhost or an IP address")
	}

	port, err := parseBoundedInt("PORT", defaultPort, 1, 65535)
	if err != nil {
		return serverConfig{}, err
	}
	authToken := os.Getenv("AUTH_TOKEN")
	adminPassword := os.Getenv("ADMIN_PASSWORD")
	if authToken == "" || adminPassword == "" {
		return serverConfig{}, fmt.Errorf("AUTH_TOKEN and ADMIN_PASSWORD are required")
	}
	if err := validateCredential("AUTH_TOKEN", authToken); err != nil {
		return serverConfig{}, err
	}
	if err := validateCredential("ADMIN_PASSWORD", adminPassword); err != nil {
		return serverConfig{}, err
	}
	if authToken != "" && constantTimeStringEqual(authToken, adminPassword) {
		return serverConfig{}, fmt.Errorf("AUTH_TOKEN and ADMIN_PASSWORD must be different")
	}
	databasePath := strings.TrimSpace(os.Getenv("DB_PATH"))
	if databasePath == "" {
		databasePath = "data.db"
	}
	return serverConfig{
		address:      net.JoinHostPort(bindAddress, strconv.Itoa(int(port))),
		databasePath: databasePath,
	}, nil
}

func parseBoundedInt(name string, fallback, minimum, maximum int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be between %d and %d", name, minimum, maximum)
	}
	return value, nil
}

func validBindAddress(address string) bool {
	return strings.EqualFold(address, "localhost") || net.ParseIP(strings.Trim(address, "[]")) != nil
}

func validateCredential(name, value string) error {
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must not have leading or trailing whitespace", name)
	}
	return nil
}

func constantTimeStringEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var difference byte
	for index := range a {
		difference |= a[index] ^ b[index]
	}
	return difference == 0
}

func serve(ctx context.Context, server *http.Server) error {
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			_ = server.Close()
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}
		err := <-serverErrors
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP during shutdown: %w", err)
		}
		return nil
	}
}

func setupRoutes(r *gin.Engine) error {
	staticFS, err := fs.Sub(embeddedWebFiles, "web/static")
	if err != nil {
		return fmt.Errorf("load embedded static assets: %w", err)
	}
	templates, err := template.ParseFS(embeddedWebFiles, "web/templates/*")
	if err != nil {
		return fmt.Errorf("parse embedded templates: %w", err)
	}
	r.StaticFS("/static", http.FS(staticFS))
	r.HTMLRender = render.HTMLProduction{Template: templates}

	r.GET("/", func(c *gin.Context) {
		c.HTML(200, "index.html", nil)
	})

	// Zencoder OAuth callback is protected by a one-time state embedded in
	// the path, so it must remain reachable from the external login page.
	oauthHandler := handler.NewOAuthHandler()
	r.GET("/oauth/zencoder/callback/:state", oauthHandler.ZencoderCallback)
	r.POST("/api/admin/session", middleware.CreateAdminSession())
	r.GET("/api/admin/session", middleware.ResumeAdminSession())
	r.DELETE("/api/admin/session", middleware.DestroyAdminSession())
	apiAuth := middleware.AuthMiddleware()

	// Anthropic API - /v1/messages
	anthropicHandler := handler.NewAnthropicHandler()
	r.POST("/v1/messages", apiAuth, anthropicHandler.Messages)

	// OpenAI API - /v1/chat/completions, /v1/responses
	openaiHandler := handler.NewOpenAIHandler()
	r.GET("/v1/models", apiAuth, openaiHandler.Models)
	r.GET("/v1/models/:model", apiAuth, openaiHandler.Model)
	r.POST("/v1/chat/completions", apiAuth, openaiHandler.ChatCompletions)
	r.POST("/v1/responses", apiAuth, openaiHandler.Responses)

	// Gemini API - /v1beta/models/*path
	geminiHandler := handler.NewGeminiHandler()
	r.GET("/v1beta/models", apiAuth, geminiHandler.ListModels)
	r.GET("/v1beta/models/:model", apiAuth, geminiHandler.GetModel)
	r.POST("/v1beta/models/*path", apiAuth, geminiHandler.HandleRequest)

	// Account management API - 需要后台管理密码验证
	accountHandler := handler.NewAccountHandler()
	api := r.Group("/api")
	api.Use(middleware.AdminAuthMiddleware()) // 应用后台管理密码验证中间件
	{
		// 账号管理
		api.GET("/accounts", accountHandler.List)
		api.POST("/accounts/credits/refresh", accountHandler.RefreshCredits)
		api.POST("/accounts/api-key", accountHandler.CreateAPIKey)
		api.PUT("/accounts/:id/api-key", accountHandler.RotateAPIKey)
		api.DELETE("/accounts/:id", accountHandler.Delete)
		api.POST("/accounts/batch/delete", accountHandler.BatchDelete)
		api.POST("/oauth/zencoder/start", oauthHandler.StartZencoder)
		api.POST("/oauth/zencoder/complete", oauthHandler.CompleteZencoder)
	}
	return nil
}
