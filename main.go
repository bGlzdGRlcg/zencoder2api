package main

import (
	"os"

	"zencoder-2api/internal/database"
	"zencoder-2api/internal/handler"
	"zencoder-2api/internal/logging"
	"zencoder-2api/internal/middleware"
	"zencoder-2api/internal/model"
	"zencoder-2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

func main() {
	// Keep the proxy quiet by default (LOG_LEVEL=silent). Set LOG_LEVEL to
	// "error", "warn", "info", or "debug" to enable progressive verbosity.
	// Startup fatals always reach stderr regardless of LOG_LEVEL.
	logging.Init()
	gin.SetMode(gin.ReleaseMode)

	// Validate the built-in model catalog at startup so misconfigurations
	// (duplicate IDs, missing required fields) are surfaced early.
	for _, ve := range model.ValidateCatalog() {
		logging.Errorf("Model catalog: %s: %s", ve.ModelID, ve.Issue)
	}

	// 加载 .env 文件
	_ = godotenv.Load()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "data.db"
	}

	if err := database.Init(dbPath); err != nil {
		logging.Fatalf("Failed to init database: %v", err)
	}

	// 启动积分重置定时任务
	service.StartCreditResetScheduler()

	// 初始化账号池
	service.InitAccountPool()

	r := gin.New()
	r.Use(gin.Recovery())
	setupRoutes(r)

	if err := r.Run(":" + port); err != nil {
		return
	}
}

func setupRoutes(r *gin.Engine) {
	r.Static("/static", "./web/static")
	r.LoadHTMLGlob("web/templates/*")

	r.GET("/", func(c *gin.Context) {
		c.HTML(200, "index.html", nil)
	})

	// Zencoder OAuth callback is protected by a one-time PKCE state embedded in
	// the path, so it must remain reachable from the external login page.
	oauthHandler := handler.NewOAuthHandler()
	r.GET("/oauth/zencoder/callback/:state", oauthHandler.ZencoderCallback)

	// Anthropic API - /v1/messages
	anthropicHandler := handler.NewAnthropicHandler()
	r.POST("/v1/messages", middleware.AuthMiddleware(), anthropicHandler.Messages)

	// OpenAI API - /v1/chat/completions, /v1/responses
	openaiHandler := handler.NewOpenAIHandler()
	r.GET("/v1/models", middleware.AuthMiddleware(), openaiHandler.Models)
	r.POST("/v1/chat/completions", middleware.AuthMiddleware(), openaiHandler.ChatCompletions)
	r.POST("/v1/responses", middleware.AuthMiddleware(), openaiHandler.Responses)

	// Gemini API - /v1beta/models/*path
	geminiHandler := handler.NewGeminiHandler()
	r.GET("/v1beta/models", middleware.AuthMiddleware(), geminiHandler.ListModels)
	r.POST("/v1beta/models/*path", middleware.AuthMiddleware(), geminiHandler.HandleRequest)

	// Account management API - 需要后台管理密码验证
	accountHandler := handler.NewAccountHandler()
	api := r.Group("/api")
	api.Use(middleware.AdminAuthMiddleware()) // 应用后台管理密码验证中间件
	{
		// 账号管理
		api.GET("/accounts", accountHandler.List)
		api.DELETE("/accounts/:id", accountHandler.Delete)
		api.POST("/accounts/batch/delete", accountHandler.BatchDelete)
		api.POST("/oauth/zencoder/start", oauthHandler.StartZencoder)
	}
}
