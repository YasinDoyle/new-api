package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
)

var (
	requestEstimateRouter      *gin.Engine
	requestEstimateRouteMu     sync.RWMutex
	requestEstimateRouteFormat map[string]types.RelayFormat
)

func resetRequestEstimateRouter() {
	requestEstimateRouteMu.Lock()
	defer requestEstimateRouteMu.Unlock()
	requestEstimateRouter = gin.New()
	requestEstimateRouteFormat = make(map[string]types.RelayFormat)
}

func registerRequestEstimateRoute(method, path string, relayFormat types.RelayFormat) {
	requestEstimateRouteMu.Lock()
	defer requestEstimateRouteMu.Unlock()
	if requestEstimateRouter == nil {
		return
	}
	key := method + " " + path
	if existing, ok := requestEstimateRouteFormat[key]; ok {
		if existing != relayFormat {
			panic("request estimate route has conflicting relay format: " + key)
		}
		return
	}
	requestEstimateRouter.Handle(method, path, func(c *gin.Context) {
		c.Header("X-Request-Estimate-Relay-Format", string(relayFormat))
	})
	requestEstimateRouteFormat[key] = relayFormat
}

func resolveRequestEstimateRoute(method, path string) (types.RelayFormat, bool) {
	requestEstimateRouteMu.RLock()
	defer requestEstimateRouteMu.RUnlock()
	if requestEstimateRouter == nil || isGeminiCountTokensPath(path) {
		return "", false
	}
	request := httptest.NewRequest(method, path, nil)
	response := httptest.NewRecorder()
	requestEstimateRouter.ServeHTTP(response, request)
	relayFormat := types.RelayFormat(response.Header().Get("X-Request-Estimate-Relay-Format"))
	return relayFormat, relayFormat != ""
}

func isGeminiCountTokensPath(path string) bool {
	return strings.HasSuffix(path, ":countTokens")
}

func registerRelayEndpoint(router *gin.RouterGroup, method, path string, relayFormat types.RelayFormat) {
	registerRequestEstimateRoute(method, router.BasePath()+path, relayFormat)
	router.Handle(method, path, func(c *gin.Context) { controller.Relay(c, relayFormat) })
}

func SetRelayRouter(router *gin.Engine) {
	resetRequestEstimateRouter()
	router.Use(middleware.CORS())
	router.Use(middleware.DecompressRequestMiddleware())
	router.Use(middleware.BodyStorageCleanup()) // 清理请求体存储
	router.Use(middleware.StatsMiddleware())
	const chatPath = "/chat/completions"
	router.POST("/v1/requests/estimate", middleware.EstimateRateLimit(), middleware.EstimateTokenAuth(), middleware.EstimateTarget(resolveRequestEstimateRoute), middleware.DistributeForEstimate(), func(c *gin.Context) {
		relayFormat, _ := c.Get("estimate_relay_format")
		controller.RequestEstimate(c, relayFormat.(types.RelayFormat))
	})
	// https://platform.openai.com/docs/api-reference/introduction
	modelsRouter := router.Group("/v1/models")
	modelsRouter.Use(middleware.RouteTag("relay"))
	modelsRouter.Use(middleware.TokenAuth())
	{
		modelsRouter.GET("", func(c *gin.Context) {
			switch {
			case c.GetHeader("x-api-key") != "" && c.GetHeader("anthropic-version") != "":
				controller.ListModels(c, constant.ChannelTypeAnthropic)
			case c.GetHeader("x-goog-api-key") != "" || c.Query("key") != "": // 单独的适配
				controller.ListModels(c, constant.ChannelTypeGemini)
			default:
				controller.ListModels(c, constant.ChannelTypeOpenAI)
			}
		})

		modelsRouter.GET("/:model", func(c *gin.Context) {
			switch {
			case c.GetHeader("x-api-key") != "" && c.GetHeader("anthropic-version") != "":
				controller.RetrieveModel(c, constant.ChannelTypeAnthropic)
			default:
				controller.RetrieveModel(c, constant.ChannelTypeOpenAI)
			}
		})
	}

	geminiRouter := router.Group("/v1beta/models")
	geminiRouter.Use(middleware.RouteTag("relay"))
	geminiRouter.Use(middleware.TokenAuth())
	{
		geminiRouter.GET("", func(c *gin.Context) {
			controller.ListModels(c, constant.ChannelTypeGemini)
		})
	}

	geminiCompatibleRouter := router.Group("/v1beta/openai/models")
	geminiCompatibleRouter.Use(middleware.RouteTag("relay"))
	geminiCompatibleRouter.Use(middleware.TokenAuth())
	{
		geminiCompatibleRouter.GET("", func(c *gin.Context) {
			controller.ListModels(c, constant.ChannelTypeOpenAI)
		})
	}

	playgroundRouter := router.Group("/pg")
	playgroundRouter.Use(middleware.RouteTag("relay"))
	playgroundRouter.Use(middleware.SystemPerformanceCheck())
	playgroundRouter.Use(middleware.UserAuth(), middleware.Distribute())
	{
		playgroundRouter.POST("/chat/completions", controller.Playground)
	}
	relayV1Router := router.Group("/v1")
	relayV1Router.Use(middleware.RouteTag("relay"))
	relayV1Router.Use(middleware.SystemPerformanceCheck())
	relayV1Router.Use(middleware.TokenAuth())
	{
		// Responses WebSocket route. Channel selection happens after the first
		// response.create event; each event runs the ordinary request limiter.
		relayV1Router.GET("/responses", controller.ResponsesWebSocket)
	}
	relayV1Router.Use(middleware.ModelRequestRateLimit())
	{
		// WebSocket 路由（统一到 Relay）
		wsRouter := relayV1Router.Group("")
		wsRouter.Use(middleware.Distribute())
		wsRouter.GET("/realtime", func(c *gin.Context) {
			controller.Relay(c, types.RelayFormatOpenAIRealtime)
		})
	}
	{
		//http router
		httpRouter := relayV1Router.Group("")
		httpRouter.Use(middleware.Distribute())

		// claude related routes
		// TODO: /messages/count_tokens is disabled. The current controller.CountClaudeTokens
		// httpRouter.POST("/messages/count_tokens", controller.CountClaudeTokens)
		registerRelayEndpoint(httpRouter, http.MethodPost, "/messages", types.RelayFormatClaude)

		// chat related routes
		registerRelayEndpoint(httpRouter, http.MethodPost, "/completions", types.RelayFormatOpenAI)
		registerRelayEndpoint(httpRouter, http.MethodPost, chatPath, types.RelayFormatOpenAI)

		// response related routes
		registerRelayEndpoint(httpRouter, http.MethodPost, "/responses/compact", types.RelayFormatOpenAIResponsesCompaction)

		// alpha search related routes (Codex standalone web search)
		registerRelayEndpoint(httpRouter, http.MethodPost, "/alpha/search", types.RelayFormatOpenAIAlphaSearch)

		// DeepSeek Search and Fetch API routes
		registerRelayEndpoint(httpRouter, http.MethodPost, "/tools/search", types.RelayFormatDeepSeekTools)
		registerRelayEndpoint(httpRouter, http.MethodPost, "/tools/fetch", types.RelayFormatDeepSeekTools)

		// image related routes. /images/generations and /images/edits are
		// host protocol endpoints (openai_image) registered by
		// SetTaskPluginProtocolRouter; unclaimed models fall back to Relay.
		httpRouter.POST("/edits", func(c *gin.Context) {
			controller.Relay(c, types.RelayFormatOpenAIImage)
		})

		// embedding related routes
		registerRelayEndpoint(httpRouter, http.MethodPost, "/embeddings", types.RelayFormatEmbedding)

		// audio related routes
		httpRouter.POST("/audio/transcriptions", func(c *gin.Context) {
			controller.Relay(c, types.RelayFormatOpenAIAudio)
		})
		httpRouter.POST("/audio/translations", func(c *gin.Context) {
			controller.Relay(c, types.RelayFormatOpenAIAudio)
		})
		httpRouter.POST("/audio/speech", func(c *gin.Context) {
			controller.Relay(c, types.RelayFormatOpenAIAudio)
		})

		// rerank related routes
		registerRelayEndpoint(httpRouter, http.MethodPost, "/rerank", types.RelayFormatRerank)

		// gemini relay routes
		registerRelayEndpoint(httpRouter, http.MethodPost, "/engines/:model/embeddings", types.RelayFormatGemini)
		registerRelayEndpoint(httpRouter, http.MethodPost, "/models/*path", types.RelayFormatGemini)

		// other relay routes
		registerRelayEndpoint(httpRouter, http.MethodPost, "/moderations", types.RelayFormatOpenAI)

		// not implemented
		httpRouter.POST("/images/variations", controller.RelayNotImplemented)
		httpRouter.GET("/files", controller.RelayNotImplemented)
		httpRouter.POST("/files", controller.RelayNotImplemented)
		httpRouter.DELETE("/files/:id", controller.RelayNotImplemented)
		httpRouter.GET("/files/:id", controller.RelayNotImplemented)
		httpRouter.GET("/files/:id/content", controller.RelayNotImplemented)
		httpRouter.POST("/fine-tunes", controller.RelayNotImplemented)
		httpRouter.GET("/fine-tunes", controller.RelayNotImplemented)
		httpRouter.GET("/fine-tunes/:id", controller.RelayNotImplemented)
		httpRouter.POST("/fine-tunes/:id/cancel", controller.RelayNotImplemented)
		httpRouter.GET("/fine-tunes/:id/events", controller.RelayNotImplemented)
		httpRouter.DELETE("/models/:model", controller.RelayNotImplemented)
	}

	relayMjRouter := router.Group("/mj")
	relayMjRouter.Use(middleware.RouteTag("relay"))
	relayMjRouter.Use(middleware.SystemPerformanceCheck())
	registerMjRouterGroup(relayMjRouter)

	relayMjModeRouter := router.Group("/:mode/mj")
	relayMjModeRouter.Use(middleware.RouteTag("relay"))
	relayMjModeRouter.Use(middleware.SystemPerformanceCheck())
	registerMjRouterGroup(relayMjModeRouter)
	//relayMjRouter.Use()

	relayGeminiRouter := router.Group("/v1beta")
	// :countTokens is not implemented. Answer it like an unregistered route
	// before auth/channel selection instead of silently relaying it as
	// generateContent (#7283).
	relayGeminiRouter.Use(func(c *gin.Context) {
		if isGeminiCountTokensPath(c.Request.URL.Path) {
			controller.RelayNotFound(c)
			c.Abort()
		}
	})
	relayGeminiRouter.Use(middleware.RouteTag("relay"))
	relayGeminiRouter.Use(middleware.SystemPerformanceCheck())
	relayGeminiRouter.Use(middleware.TokenAuth())
	relayGeminiRouter.Use(middleware.ModelRequestRateLimit())
	relayGeminiRouter.Use(middleware.Distribute())
	{
		// Gemini API 路径格式: /v1beta/models/{model_name}:{action}
		registerRelayEndpoint(relayGeminiRouter, http.MethodPost, "/models/*path", types.RelayFormatGemini)
	}
}

func registerMjRouterGroup(relayMjRouter *gin.RouterGroup) {
	relayMjRouter.GET("/image/:id", relay.RelayMidjourneyImage)
	relayMjRouter.Use(middleware.TokenAuth(), middleware.Distribute())
	{
		relayMjRouter.POST("/submit/action", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/shorten", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/modal", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/imagine", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/change", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/simple-change", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/describe", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/blend", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/edits", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/video", controller.RelayMidjourney)
		//relayMjRouter.POST("/notify", controller.RelayMidjourney)
		relayMjRouter.GET("/task/:id/fetch", controller.RelayMidjourney)
		relayMjRouter.GET("/task/:id/image-seed", controller.RelayMidjourney)
		relayMjRouter.POST("/task/list-by-condition", controller.RelayMidjourney)
		relayMjRouter.POST("/insight-face/swap", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/upload-discord-images", controller.RelayMidjourney)
	}
}
