package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/logger"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// PrepareRequestEstimateNativeRoute decodes a dynamically matched native
// submit route using the same plugin hook and intent checks as live routing.
func PrepareRequestEstimateNativeRoute() gin.HandlerFunc {
	return func(c *gin.Context) {
		value, exists := c.Get(pluginruntime.ContextKeyPinnedRoute)
		if !exists {
			c.Next()
			return
		}
		pinned, ok := value.(pluginruntime.PinnedRoute)
		if !ok || pinned.Generation == nil || pinned.Plugin == nil || pinned.Route.Type == pluginruntime.RouteTypeQuery {
			abortWithOpenAiMessage(c, http.StatusUnprocessableEntity, "Request estimation is not supported for this operation")
			return
		}
		requestContext, err := buildTaskPluginRouteRequest(c)
		if err != nil {
			status := invalidRequestStatus(err)
			if errors.Is(err, errTaskPluginUnsupportedMediaType) {
				status = http.StatusUnsupportedMediaType
			}
			abortWithOpenAiMessage(c, status, "Invalid task plugin request")
			return
		}
		if params, exists := c.Get(requestEstimateNativeParamsKey); exists {
			if routeParams, ok := params.(map[string]string); ok {
				requestContext.Params = routeParams
			}
		}
		body, _ := requestContext.Body.(map[string]any)
		kind, _ := body["kind"].(string)
		if kind != string(pluginruntime.BodyJSON) && kind != string(pluginruntime.BodyMultipart) {
			abortWithOpenAiMessage(c, http.StatusUnsupportedMediaType, "This route requires a JSON or multipart body")
			return
		}
		if len(pinned.Route.Models) > 0 {
			bodyValue, _ := body["value"].(map[string]any)
			modelName, _ := bodyValue["model"].(string)
			if !slices.Contains(pinned.Route.Models, modelName) {
				abortWithOpenAiMessage(c, http.StatusBadRequest, fmt.Sprintf("model %q is not allowed on this route", modelName))
				return
			}
		}
		resolvedValue, requestBodyText, err := pinned.Plugin.Engine.CallPathWithMemberJSON(
			c.Request.Context(), 0, pinned.Plugin.Meta.JSONTextMember("requestBody"),
			"native", []string{pinned.Route.Decode}, requestContext.JSValueFor(pinned.Plugin.Meta),
		)
		if err != nil {
			message := taskPluginHookDetail(err)
			if message == "" {
				message = "Invalid task plugin request"
			}
			abortWithOpenAiMessage(c, http.StatusBadRequest, message)
			return
		}
		resolved, ok := resolvedValue.(map[string]any)
		if !ok || resolved["kind"] != string(pluginruntime.RouteTypeSubmit) {
			abortWithOpenAiMessage(c, http.StatusBadRequest, taskPluginInvalidRouteResult)
			return
		}
		modelName, ok := resolved["model"].(string)
		if !ok || strings.TrimSpace(modelName) == "" || !slices.Contains(pinned.Plugin.Meta.Models, modelName) {
			abortWithOpenAiMessage(c, http.StatusBadRequest, "Decoded model is not served by this plugin")
			return
		}
		if len(pinned.Route.Models) > 0 && !slices.Contains(pinned.Route.Models, modelName) {
			abortWithOpenAiMessage(c, http.StatusBadRequest, fmt.Sprintf("model %q is not allowed on this route", modelName))
			return
		}
		if _, forbidden := resolved["renderer"]; forbidden {
			abortWithOpenAiMessage(c, http.StatusBadRequest, taskPluginInvalidRouteResult)
			return
		}
		requestContext.RequestBody = resolved["requestBody"]
		action := pinned.Route.Action
		if resolvedAction, present := resolved["action"]; present {
			actionValue, valid := resolvedAction.(string)
			if !valid {
				abortWithOpenAiMessage(c, http.StatusBadRequest, taskPluginInvalidRouteResult)
				return
			}
			if strings.TrimSpace(actionValue) != "" {
				action = actionValue
			}
		}
		c.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{Generation: pinned.Generation, Plugin: pinned.Plugin})
		c.Set(pluginruntime.ContextKeyRouteRequest, requestContext)
		c.Set("task_request", requestContext.RequestBody)
		if requestBodyText != nil {
			c.Set(pluginruntime.ContextKeyRequestBodyText, requestBodyText)
		}
		c.Set("resolved_task_model", modelName)
		c.Set("expected_task_plugin_key", pinned.Plugin.Meta.Key)
		c.Set("task_plugin_key", pinned.Plugin.Meta.Key)
		c.Set("platform", pinned.Plugin.Meta.Key)
		if action != "" {
			c.Set("task_action", action)
		}
		c.Set("relay_mode", relayconstant.RelayModeVideoSubmit)
		service.AppendTaskPluginIdentityFilter(c, pinned.Plugin.Meta.Key)
		if intentErr := applyOriginTaskIntent(c, resolved, pinned.Plugin.Meta); intentErr != nil {
			abortWithOpenAiMessage(c, intentErr.StatusCode, intentErr.Message)
			return
		}
		logger.LogDebug(c, "request_estimate subsystem=plugin_route generation=%d plugin=%q route=%q model=%q", pinned.Generation.Number, pinned.Plugin.Meta.Key, pinned.Route.Path, modelName)
		c.Next()
	}
}
