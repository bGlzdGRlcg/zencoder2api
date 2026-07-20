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
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"zencoder-2api/internal/database"
	"zencoder-2api/internal/handler"
	"zencoder-2api/internal/logging"
	"zencoder-2api/internal/middleware"
	"zencoder-2api/internal/model"
	"zencoder-2api/internal/secret"
	"zencoder-2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/render"
	"github.com/joho/godotenv"
)

//go:embed web/templates/* web/static/*
var embeddedWebFiles embed.FS

const (
	defaultPort             = 8080
	defaultMaxRequestBytes  = 4 << 20
	maximumMaxRequestBytes  = 128 << 20
	maxInFlightRequestBytes = 512 << 20
	minimumCredentialLength = 16
	readinessCacheTTL       = time.Second
	readinessProbeTimeout   = time.Second
	streamWriteIdleTimeout  = 30 * time.Second
	shutdownTimeout         = 30 * time.Second
)

type serverConfig struct {
	address             string
	databasePath        string
	maxRequestBytes     int64
	maxConcurrent       int
	requestsPerMinute   int
	adminLoginPerMinute int
	trustedProxies      []string
}

func main() {
	if err := run(); err != nil {
		logging.Fatalf("%v", err)
	}
}

func run() error {
	// Load local development configuration before any package reads the
	// environment. A missing .env is normal; a malformed one is not.
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load .env: %w", err)
	}

	// Keep the proxy quiet by default (LOG_LEVEL=silent). Set LOG_LEVEL to
	// "error", "warn", "info", or "debug" to enable progressive verbosity.
	// Startup fatals always reach stderr regardless of LOG_LEVEL.
	logging.Init()
	gin.SetMode(gin.ReleaseMode)

	cfg, err := loadServerConfig()
	if err != nil {
		return err
	}
	if err := service.ValidateCreditResetConfig(); err != nil {
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

	r := gin.New()
	r.Use(middleware.Recovery())
	r.Use(middleware.RequestID())
	r.Use(middleware.SecurityHeaders())
	r.Use(middleware.WriteIdleTimeout(streamWriteIdleTimeout))
	r.Use(middleware.BodyLimit(cfg.maxRequestBytes))
	if err := r.SetTrustedProxies(cfg.trustedProxies); err != nil {
		return fmt.Errorf("configure trusted proxies: %w", err)
	}
	liveness := func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
	readiness := newReadinessHandler(func(ctx context.Context) bool {
		if err := sqlDB.PingContext(ctx); err != nil {
			return false
		}
		return service.AccountPoolReadyContext(ctx)
	})
	r.GET("/livez", liveness)
	r.GET("/readyz", readiness)
	// Keep the existing health endpoint as a readiness-compatible alias while
	// Docker/Kubernetes liveness checks use /livez and never depend on accounts.
	r.GET("/healthz", readiness)
	if err := setupRoutes(r, cfg); err != nil {
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
	allowInsecure, err := parseOptionalBool("ALLOW_INSECURE_LOCALHOST")
	if err != nil {
		return serverConfig{}, err
	}

	bindAddress := strings.TrimSpace(os.Getenv("BIND_ADDRESS"))
	if bindAddress == "" {
		if allowInsecure {
			bindAddress = "127.0.0.1"
		} else {
			bindAddress = "0.0.0.0"
		}
	}
	if !validBindAddress(bindAddress) {
		return serverConfig{}, fmt.Errorf("BIND_ADDRESS must be localhost or an IP address")
	}

	port, err := parseBoundedInt("PORT", defaultPort, 1, 65535)
	if err != nil {
		return serverConfig{}, err
	}
	maxRequestBytes, err := parseBoundedInt("MAX_REQUEST_BODY_BYTES", defaultMaxRequestBytes, 1024, maximumMaxRequestBytes)
	if err != nil {
		return serverConfig{}, err
	}

	authToken := os.Getenv("AUTH_TOKEN")
	adminPassword := os.Getenv("ADMIN_PASSWORD")
	missingCredentials := authToken == "" || adminPassword == ""
	if allowInsecure && !isLoopbackAddress(bindAddress) {
		return serverConfig{}, fmt.Errorf("ALLOW_INSECURE_LOCALHOST requires a loopback BIND_ADDRESS")
	}
	if missingCredentials && !allowInsecure {
		return serverConfig{}, fmt.Errorf("AUTH_TOKEN and ADMIN_PASSWORD are required; set ALLOW_INSECURE_LOCALHOST=true only for loopback development")
	}
	publicBaseURL := strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL"))
	if !isLoopbackAddress(bindAddress) && publicBaseURL == "" {
		return serverConfig{}, fmt.Errorf("PUBLIC_BASE_URL is required when BIND_ADDRESS is not loopback")
	}
	if publicBaseURL != "" {
		if err := validatePublicBaseURL(publicBaseURL); err != nil {
			return serverConfig{}, err
		}
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
	if err := secret.ValidateKey(); err != nil {
		return serverConfig{}, err
	}
	maxConcurrent, err := parseBoundedInt("MAX_CONCURRENT_REQUESTS", 32, 1, 100000)
	if err != nil {
		return serverConfig{}, err
	}
	if maxRequestBytes > maxInFlightRequestBytes/maxConcurrent {
		return serverConfig{}, fmt.Errorf("MAX_REQUEST_BODY_BYTES multiplied by MAX_CONCURRENT_REQUESTS must not exceed %d bytes", maxInFlightRequestBytes)
	}
	requestsPerMinute, err := parseBoundedInt("REQUESTS_PER_MINUTE", 600, 1, 1000000)
	if err != nil {
		return serverConfig{}, err
	}
	adminLoginPerMinute, err := parseBoundedInt("ADMIN_LOGIN_REQUESTS_PER_MINUTE", 10, 1, 10000)
	if err != nil {
		return serverConfig{}, err
	}
	trustedProxies, err := parseTrustedProxies(os.Getenv("TRUSTED_PROXIES"))
	if err != nil {
		return serverConfig{}, err
	}
	if allowInsecure && publicBaseURL != "" {
		return serverConfig{}, fmt.Errorf("ALLOW_INSECURE_LOCALHOST cannot be combined with PUBLIC_BASE_URL")
	}
	if allowInsecure && len(trustedProxies) > 0 {
		return serverConfig{}, fmt.Errorf("ALLOW_INSECURE_LOCALHOST cannot be combined with TRUSTED_PROXIES")
	}

	databasePath := strings.TrimSpace(os.Getenv("DB_PATH"))
	if databasePath == "" {
		databasePath = "data.db"
	}
	return serverConfig{
		address:             net.JoinHostPort(bindAddress, strconv.Itoa(int(port))),
		databasePath:        databasePath,
		maxRequestBytes:     maxRequestBytes,
		maxConcurrent:       int(maxConcurrent),
		requestsPerMinute:   int(requestsPerMinute),
		adminLoginPerMinute: int(adminLoginPerMinute),
		trustedProxies:      trustedProxies,
	}, nil
}

func newReadinessHandler(probe func(context.Context) bool) gin.HandlerFunc {
	var cache struct {
		sync.RWMutex
		ready     bool
		expiresAt time.Time
	}
	probeGate := make(chan struct{}, 1)

	readCached := func(now time.Time) (bool, bool) {
		cache.RLock()
		defer cache.RUnlock()
		return cache.ready, now.Before(cache.expiresAt)
	}
	writeResult := func(c *gin.Context, ready bool) {
		if ready {
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
			return
		}
		c.Header("Retry-After", "1")
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
	}

	return func(c *gin.Context) {
		if ready, ok := readCached(time.Now()); ok {
			writeResult(c, ready)
			return
		}

		select {
		case probeGate <- struct{}{}:
			defer func() { <-probeGate }()
		default:
			writeResult(c, false)
			return
		}

		// Another request may have refreshed the cache while this request was
		// acquiring the single probe slot.
		if ready, ok := readCached(time.Now()); ok {
			writeResult(c, ready)
			return
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), readinessProbeTimeout)
		ready := probe(ctx)
		cancel()

		cache.Lock()
		cache.ready = ready
		cache.expiresAt = time.Now().Add(readinessCacheTTL)
		cache.Unlock()
		writeResult(c, ready)
	}
}

func parseTrustedProxies(raw string) ([]string, error) {
	var proxies []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if net.ParseIP(item) != nil {
			proxies = append(proxies, item)
			continue
		}
		_, network, err := net.ParseCIDR(item)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXIES contains invalid IP/CIDR %q", item)
		}
		if ones, bits := network.Mask.Size(); ones == 0 && bits > 0 {
			return nil, fmt.Errorf("TRUSTED_PROXIES must not contain unrestricted CIDR %q", item)
		}
		proxies = append(proxies, item)
	}
	return proxies, nil
}

func parseOptionalBool(name string) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	switch strings.ToLower(raw) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, fmt.Errorf("%s must be true or false", name)
	}
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

func isLoopbackAddress(address string) bool {
	if strings.EqualFold(address, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(address, "[]"))
	return ip != nil && ip.IsLoopback()
}

func validateCredential(name, value string) error {
	if value == "" {
		return nil
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must not have leading or trailing whitespace", name)
	}
	if strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return fmt.Errorf("%s must not contain whitespace", name)
	}
	if len(value) < minimumCredentialLength {
		return fmt.Errorf("%s must contain at least %d characters", name, minimumCredentialLength)
	}
	return nil
}

func validatePublicBaseURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("PUBLIC_BASE_URL must be a valid HTTP(S) URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return fmt.Errorf("PUBLIC_BASE_URL must be an origin without credentials, path, query parameters, or fragments")
	}
	if parsed.Scheme != "https" && !isLoopbackHost(parsed.Hostname()) {
		return fmt.Errorf("PUBLIC_BASE_URL must use HTTPS unless its host is loopback")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

func setupRoutes(r *gin.Engine, cfg serverConfig) error {
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
	oauthCallbackLimiter := middleware.NewRemoteRequestLimiter(8, cfg.adminLoginPerMinute)
	r.GET("/oauth/zencoder/callback/:state", oauthCallbackLimiter.Middleware(), oauthHandler.ZencoderCallback)
	adminLoginLimiter := middleware.NewRemoteRequestLimiter(8, cfg.adminLoginPerMinute)
	r.POST("/api/admin/session", adminLoginLimiter.Middleware(), middleware.CreateAdminSession())
	r.DELETE("/api/admin/session", middleware.DestroyAdminSession())
	// Authentication happens after this limiter, so bucket only by the remote
	// address. Untrusted credential guesses must not create unbounded identities
	// or bypass the pre-authentication request limit.
	inferenceLimiter := middleware.NewRemoteRequestLimiter(cfg.maxConcurrent, cfg.requestsPerMinute)
	apiAuth := middleware.AuthMiddleware()

	// Anthropic API - /v1/messages
	anthropicHandler := handler.NewAnthropicHandler()
	r.POST("/v1/messages", inferenceLimiter.Middleware(), apiAuth, anthropicHandler.Messages)

	// OpenAI API - /v1/chat/completions, /v1/responses
	openaiHandler := handler.NewOpenAIHandler()
	r.GET("/v1/models", inferenceLimiter.Middleware(), apiAuth, openaiHandler.Models)
	r.GET("/v1/models/:model", inferenceLimiter.Middleware(), apiAuth, openaiHandler.Model)
	r.POST("/v1/chat/completions", inferenceLimiter.Middleware(), apiAuth, openaiHandler.ChatCompletions)
	r.POST("/v1/responses", inferenceLimiter.Middleware(), apiAuth, openaiHandler.Responses)

	// Gemini API - /v1beta/models/*path
	geminiHandler := handler.NewGeminiHandler()
	r.GET("/v1beta/models", inferenceLimiter.Middleware(), apiAuth, geminiHandler.ListModels)
	r.GET("/v1beta/models/:model", inferenceLimiter.Middleware(), apiAuth, geminiHandler.GetModel)
	r.POST("/v1beta/models/*path", inferenceLimiter.Middleware(), apiAuth, geminiHandler.HandleRequest)

	// Account management API - 需要后台管理密码验证
	accountHandler := handler.NewAccountHandler()
	api := r.Group("/api")
	api.Use(middleware.AdminAuthMiddleware()) // 应用后台管理密码验证中间件
	{
		// 账号管理
		api.GET("/accounts", accountHandler.List)
		api.POST("/accounts/api-key", accountHandler.CreateAPIKey)
		api.PUT("/accounts/:id/api-key", accountHandler.RotateAPIKey)
		api.DELETE("/accounts/:id", accountHandler.Delete)
		api.POST("/accounts/batch/delete", accountHandler.BatchDelete)
		api.POST("/oauth/zencoder/start", oauthHandler.StartZencoder)
		api.POST("/oauth/zencoder/complete", oauthHandler.CompleteZencoder)
	}
	return nil
}
