package router

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This is the public acceptance seam shared by the estimate tickets. The
// optional DSN points only at a disposable integration-test database.
func setupRequestEstimateDB(t *testing.T) {
	t.Helper()
	i18n.Init()
	setupRelayRouterTestDB(t)
	if dsn := os.Getenv("REQUEST_ESTIMATE_TEST_DSN"); dsn != "" {
		// Refuse an accidental production DSN before any destructive fixture setup.
		if strings.HasPrefix(dsn, "postgres://") {
			target, err := url.Parse(dsn)
			require.NoError(t, err)
			require.Equal(t, "127.0.0.1", target.Hostname())
			require.Equal(t, "/estimate01", target.Path)
		} else {
			target, err := mysql.ParseDSN(dsn)
			require.NoError(t, err)
			require.Equal(t, "estimate01", target.DBName)
			require.True(t, strings.HasPrefix(target.Addr, "127.0.0.1:"))
		}
		db, err := model.DB.DB()
		require.NoError(t, err)
		require.NoError(t, db.Close())
		t.Setenv("SQL_DSN", dsn)
		require.NoError(t, model.InitDB())
		model.LOG_DB = model.DB
		// Each run owns the entire disposable database; start from empty fixtures.
		for _, table := range []any{&model.Ability{}, &model.Token{}, &model.User{}, &model.Channel{}, &model.Log{}, &model.Task{}, &model.UserSubscription{}, &model.SubscriptionPreConsumeRecord{}} {
			require.NoError(t, model.DB.Migrator().DropTable(table))
		}
	}
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.Ability{}, &model.Channel{}, &model.Log{}, &model.Task{}, &model.UserSubscription{}, &model.SubscriptionPreConsumeRecord{}))
	var version string
	query := "SELECT version()"
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		query = "SELECT sqlite_version()"
	}
	require.NoError(t, model.DB.Raw(query).Scan(&version).Error)
	t.Logf("estimate database: %s %s", common.MainDatabaseType(), version)
}

func TestRequestEstimateContract(t *testing.T) {
	setupRequestEstimateDB(t)
	savedConfig := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(k, v string) error { savedConfig[k] = v; return nil }))
	savedPrice := ratio_setting.ModelPrice2JSONString()
	savedRatio := ratio_setting.ModelRatio2JSONString()
	savedMemory, savedCount := common.MemoryCacheEnabled, constant.CountToken
	common.MemoryCacheEnabled, constant.CountToken = false, false
	t.Cleanup(func() {
		common.MemoryCacheEnabled, constant.CountToken = savedMemory, savedCount
		require.NoError(t, config.GlobalConfig.LoadFromDB(savedConfig))
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(savedPrice))
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(savedRatio))
	})
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"estimate-chat":0.01}`))
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusBadGateway) }))
	t.Cleanup(upstream.Close)
	user := model.User{Username: "contract-user", Status: common.UserStatusEnabled, Group: "default", Quota: 0}
	require.NoError(t, model.DB.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "contractkey", Status: common.TokenStatusExhausted, ExpiredTime: -1}
	require.NoError(t, model.DB.Create(&token).Error)
	channel := model.Channel{Type: constant.ChannelTypeOpenAI, Key: "secret-one\nsecret-two", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled, Models: "estimate-chat,estimate-audio,gpt-4o,deepseek-search,deepseek-fetch", Group: "default", ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}}
	require.NoError(t, model.DB.Create(&channel).Error)
	for _, modelName := range []string{"estimate-chat", "estimate-audio", "gpt-4o", "deepseek-search", "deepseek-fetch"} {
		require.NoError(t, model.DB.Create(&model.Ability{Group: "default", Model: modelName, ChannelId: channel.Id, Enabled: true}).Error)
	}
	engine := gin.New()
	require.NoError(t, engine.SetTrustedProxies(nil))
	SetRelayRouter(engine)
	SetTaskPluginProtocolRouter(engine)
	body := `{"model":"estimate-chat","messages":[{"role":"user","content":"hello"}],"max_tokens":0}`
	envelope := map[string]any{"method": "POST", "path": "/v1/chat/completions", "body": common.RawMessage(body)}
	// Snapshot all persisted business state, including empty accounting tables.
	snapshot := func() map[string]string {
		state := map[string]string{}
		for _, table := range []string{"users", "tokens", "channels", "abilities", "logs", "tasks", "user_subscriptions", "subscription_pre_consume_records"} {
			var rows []map[string]any
			require.NoError(t, model.DB.Table(table).Find(&rows).Error)
			data, err := common.Marshal(rows)
			require.NoError(t, err)
			state[table] = string(data)
		}
		return state
	}
	quoteWithContext := func(auth string, ctx context.Context) *httptest.ResponseRecorder {
		data, err := common.Marshal(envelope)
		require.NoError(t, err)
		before := snapshot()
		r := httptest.NewRequest(http.MethodPost, "/v1/requests/estimate", strings.NewReader(string(data))).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+auth)
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = "192.0.2.10:1234"
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, r)
		assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		assert.Equal(t, before, snapshot(), "estimate must not persist business state")
		assert.Zero(t, calls.Load(), "estimate must never call the model")
		assert.NotContains(t, w.Body.String(), "secret-one")
		assert.NotContains(t, w.Body.String(), upstream.URL)
		return w
	}
	quote := func(auth string) *httptest.ResponseRecorder {
		return quoteWithContext(auth, context.Background())
	}
	t.Run("identity restrictions survive inner credentials", func(t *testing.T) {
		unauthorized := httptest.NewRecorder()
		engine.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/v1/requests/estimate", strings.NewReader(`{}`)))
		assert.Equal(t, http.StatusUnauthorized, unauthorized.Code)
		assert.Equal(t, "no-store", unauthorized.Header().Get("Cache-Control"))
		envelope["headers"] = http.Header{"Authorization": {"Bearer administrator-1"}, "X-Forwarded-For": {"127.0.0.1"}, "Cookie": {"session=forged"}}
		for _, tc := range []struct {
			name   string
			change func()
			auth   string
			status int
		}{
			{"unknown", func() {}, "unknown", 401},
			{"exhausted", func() {}, token.Key, 200},
			{"expired", func() { token.Status = common.TokenStatusEnabled; token.ExpiredTime = 1 }, token.Key, 401},
			{"disabled", func() { token.Status = common.TokenStatusDisabled }, token.Key, 401},
			{"banned user", func() { user.Status = common.UserStatusDisabled }, token.Key, 403},
			{"model limit", func() { token.ModelLimitsEnabled = true; token.ModelLimits = "other-model" }, token.Key, 403},
			{"group limit", func() { token.Group = "forbidden-group" }, token.Key, 403},
			{"IP spoof", func() { token.AllowIps = new("127.0.0.1") }, token.Key, 403},
			{"nonadmin pin", func() {}, token.Key + "-1", 403},
		} {
			t.Run(tc.name, func(t *testing.T) {
				token.Status, token.ExpiredTime, token.ModelLimitsEnabled, token.ModelLimits, token.Group, token.AllowIps = common.TokenStatusExhausted, -1, false, "", "", nil
				user.Status = common.UserStatusEnabled
				tc.change()
				require.NoError(t, model.DB.Save(&token).Error)
				require.NoError(t, model.DB.Save(&user).Error)
				w := quote(tc.auth)
				assert.Equal(t, tc.status, w.Code, w.Body.String())
			})
		}
		delete(envelope, "headers")
	})
	token.AllowIps = nil
	require.NoError(t, model.DB.Save(&token).Error)
	t.Run("envelope and unsupported requests", func(t *testing.T) {
		for _, tc := range []struct {
			name, path string
			status     int
		}{
			{"absolute", "https://example.com/v1/chat/completions", 400},
			{"authority", "//example.com/v1/chat/completions", 400},
			{"traversal", "/v1/../v1/chat/completions", 400},
			{"encoded traversal", "/v1/%2e%2e/v1/chat/completions", 400},
			{"fragment", "/v1/chat/completions#fragment", 400},
			{"bad query", "/v1/chat/completions?x=%GG", 400},
			{"recursive", "/v1/requests/estimate", 422},
			{"realtime", "/v1/realtime", 422},
			{"unimplemented Gemini countTokens", "/v1beta/models/estimate-chat:countTokens", 422},
			{"query", "/v1/models", 422},
			{"full query", "/v1/chat/completions?x=a%2Fb&x=c&key=forged", 200},
		} {
			t.Run(tc.name, func(t *testing.T) {
				envelope["path"] = tc.path
				w := quote(token.Key)
				assert.Equal(t, tc.status, w.Code, w.Body.String())
			})
		}
		envelope["path"] = "/v1/chat/completions"
		envelope["body_base64"] = "%%%"
		assert.Equal(t, 400, quote(token.Key).Code, "mutually exclusive bodies")
		delete(envelope, "body")
		assert.Equal(t, 400, quote(token.Key).Code, "invalid base64")
		envelope["body_base64"] = base64.StdEncoding.EncodeToString([]byte(body))
		assert.Equal(t, 200, quote(token.Key).Code)
		delete(envelope, "body_base64")
		envelope["body"] = common.RawMessage(strings.Replace(body, `"max_tokens":0`, `"stream":true`, 1))
		assert.Equal(t, 200, quote(token.Key).Code, "streaming targets return a single JSON estimate")
		envelope["body"] = common.RawMessage(strings.Replace(body, `"max_tokens":0`, `"max_tokens":18446744073709551615`, 1))
		assert.Equal(t, 400, quote(token.Key).Code)
		envelope["body"] = common.RawMessage(body)
		envelope["headers"] = http.Header{"Content-Type": {"application/json-invalid"}}
		assert.Equal(t, 422, quote(token.Key).Code, "invalid media type must not be accepted as JSON")
		delete(envelope, "headers")
		envelope["body"] = common.RawMessage(`{"model":"estimate-chat","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + upstream.URL + `/image"}}]}]}`)
		assert.Equal(t, 200, quote(token.Key).Code, "multimodal inputs are accepted without fetching while measurement is disabled")
		envelope["body"] = common.RawMessage(body)
		savedMaxBody := constant.MaxRequestBodyMB
		constant.MaxRequestBodyMB = 1
		envelope["padding"] = strings.Repeat("x", 1<<20)
		assert.Equal(t, 413, quote(token.Key).Code)
		delete(envelope, "padding")
		constant.MaxRequestBodyMB = savedMaxBody
	})
	t.Run("ordinary HTTP protocols and streaming", func(t *testing.T) {
		constant.CountToken = false
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"estimate-chat":0.01,"deepseek-search":0.01,"deepseek-fetch":0.01}`))
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{}`))
		cases := []struct {
			name, path, body string
			headers          http.Header
		}{
			{"completion", "/v1/completions", `{"model":"estimate-chat","prompt":"hello"}`, nil},
			{"messages", "/v1/messages", `{"model":"estimate-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, http.Header{"Anthropic-Version": {"2023-06-01"}}},
			{"responses", "/v1/responses", `{"model":"estimate-chat","input":"hello","stream":true}`, nil},
			{"responses compact", "/v1/responses/compact", `{"model":"estimate-chat","input":"hello"}`, nil},
			{"embedding", "/v1/embeddings", `{"model":"estimate-chat","input":"hello"}`, nil},
			{"rerank", "/v1/rerank", `{"model":"estimate-chat","query":"hello","documents":["world"]}`, nil},
			{"alpha search", "/v1/alpha/search", `{"model":"estimate-chat"}`, nil},
			{"tool search", "/v1/tools/search", `{"model":"estimate-chat"}`, nil},
			{"tool fetch", "/v1/tools/fetch", `{"model":"estimate-chat"}`, nil},
			{"gemini generate", "/v1beta/models/estimate-chat:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, nil},
			{"gemini streaming", "/v1beta/models/estimate-chat:streamGenerateContent?alt=sse", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, nil},
			{"gemini embedding", "/v1beta/models/estimate-chat:embedContent", `{"content":{"parts":[{"text":"hello"}]}}`, nil},
			{"gemini batch embedding", "/v1beta/models/estimate-chat:batchEmbedContents", `{"requests":[{"model":"estimate-chat","content":{"parts":[{"text":"hello"}]}}]}`, nil},
			{"streaming chat", "/v1/chat/completions", `{"model":"estimate-chat","messages":[{"role":"user","content":"hello"}],"stream":true}`, nil},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				envelope["method"], envelope["path"] = http.MethodPost, tc.path
				envelope["body"], envelope["headers"] = common.RawMessage(tc.body), tc.headers
				w := quote(token.Key)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				var result map[string]any
				require.NoError(t, common.Unmarshal(w.Body.Bytes(), &result))
				assert.Equal(t, float64(5000), result["quota"])
				assert.Equal(t, "0.01", result["amount"])
			})
		}
		for _, tc := range []struct {
			name, path, body string
			headers          http.Header
		}{
			{"messages max_tokens", "/v1/messages", `{"model":"estimate-chat","max_tokens":18446744073709551615,"messages":[{"role":"user","content":"hello"}]}`, nil},
			{"responses max_output_tokens", "/v1/responses", `{"model":"estimate-chat","input":"hello","max_output_tokens":18446744073709551615}`, nil},
			{"gemini maxOutputTokens", "/v1beta/models/estimate-chat:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":18446744073709551615}}`, nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				envelope["method"], envelope["path"] = http.MethodPost, tc.path
				envelope["body"], envelope["headers"] = common.RawMessage(tc.body), tc.headers
				assert.Equal(t, http.StatusBadRequest, quote(token.Key).Code)
			})
		}
		for _, tc := range []struct {
			name, path, body string
		}{
			{"responses image input", "/v1/responses", `{"model":"estimate-chat","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.invalid/image.png"}]}]}`},
			{"messages image input", "/v1/messages", `{"model":"estimate-chat","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.invalid/image.png"}}]}]}`},
			{"gemini inline image", "/v1beta/models/estimate-chat:generateContent", `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}]}]}`},
			{"gemini file input", "/v1beta/models/estimate-chat:embedContent", `{"content":{"parts":[{"fileData":{"mimeType":"text/plain","fileUri":"https://example.invalid/input.txt"}}]}}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				envelope["method"], envelope["path"] = http.MethodPost, tc.path
				envelope["body"], envelope["headers"] = common.RawMessage(tc.body), nil
				assert.Equal(t, http.StatusOK, quote(token.Key).Code)
			})
		}
		envelope["method"], envelope["path"], envelope["body"], envelope["headers"] = http.MethodPost, "/v1/chat/completions", common.RawMessage(body), nil
	})
	t.Run("image pricing uses effective count without submission", func(t *testing.T) {
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"gpt-4o":0.01}`))
		envelope["method"], envelope["path"] = http.MethodPost, "/v1/images/generations"
		envelope["body"], envelope["headers"] = common.RawMessage(`{"model":"gpt-4o","prompt":"a red cube","n":1}`), nil
		result := quote(token.Key)
		require.Equal(t, http.StatusOK, result.Code, result.Body.String())
		assert.Contains(t, result.Body.String(), `"quota":5000`)
		assert.Contains(t, result.Body.String(), `"amount":"0.01"`)

		previousOverride := channel.ParamOverride
		t.Cleanup(func() {
			require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("param_override", previousOverride).Error)
			model.InitChannelCache()
		})
		override := `{"operations":[{"mode":"set","path":"n","value":3}]}`
		require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("param_override", override).Error)
		model.InitChannelCache()
		result = quote(token.Key)
		require.Equal(t, http.StatusOK, result.Code, result.Body.String())
		assert.Contains(t, result.Body.String(), `"quota":15000`)
		assert.Contains(t, result.Body.String(), `"amount":"0.03"`)
		delete(envelope, "body")
		envelope["body"] = common.RawMessage(`{"model":"gpt-4o","prompt":"a red cube","n":1,"parameters":{"n":3}}`)
		result = quote(token.Key)
		require.Equal(t, http.StatusOK, result.Code, result.Body.String())
		assert.Contains(t, result.Body.String(), `"quota":15000`)
		envelope["body"] = common.RawMessage(`{"model":"gpt-4o","prompt":"a red cube","n":1,"parameters":{"n":129}}`)
		assert.Equal(t, http.StatusBadRequest, quote(token.Key).Code, "passthrough image counts must retain the existing upper bound")
		require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("param_override", previousOverride).Error)
		model.InitChannelCache()
		var form bytes.Buffer
		writer := multipart.NewWriter(&form)
		require.NoError(t, writer.WriteField("model", "gpt-4o"))
		require.NoError(t, writer.WriteField("prompt", "edit this image"))
		require.NoError(t, writer.WriteField("n", "2"))
		file, err := writer.CreateFormFile("image", "input.png")
		require.NoError(t, err)
		_, err = file.Write([]byte("image bytes"))
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		envelope["path"] = "/v1/images/edits"
		delete(envelope, "body")
		envelope["body_base64"] = base64.StdEncoding.EncodeToString(form.Bytes())
		envelope["headers"] = http.Header{"Content-Type": {writer.FormDataContentType()}}
		result = quote(token.Key)
		require.Equal(t, http.StatusOK, result.Code, result.Body.String())
		assert.Contains(t, result.Body.String(), `"quota":10000`)
		assert.Contains(t, result.Body.String(), `"amount":"0.02"`)
		delete(envelope, "body_base64")
		delete(envelope, "headers")

		imageBillingConfig := map[string]string{}
		require.NoError(t, config.GlobalConfig.SaveToDB(func(k, v string) error {
			if k == "billing_setting.billing_mode" || k == "billing_setting.billing_expr" {
				imageBillingConfig[k] = v
			}
			return nil
		}))
		t.Cleanup(func() {
			require.NoError(t, config.GlobalConfig.LoadFromDB(imageBillingConfig))
		})
		for _, tc := range []struct {
			name, expression      string
			oneQuota, fourQuota   float64
			oneAmount, fourAmount string
		}{
			{"token-only image expression", `tier("input", p * 4)`, 0, 0, "0", "0"},
			{"per-request image expression", `tier("request", fixed(0.02))`, 10000, 10000, "0.02", "0.02"},
			{"per-image expression", `tier("image", fixed(0.02)) * image_count`, 10000, 40000, "0.02", "0.08"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				modes, err := common.Marshal(map[string]string{"gpt-4o": "tiered_expr"})
				require.NoError(t, err)
				expressions, err := common.Marshal(map[string]string{"gpt-4o": tc.expression})
				require.NoError(t, err)
				require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"billing_setting.billing_mode": string(modes), "billing_setting.billing_expr": string(expressions)}))
				envelope["path"] = "/v1/images/generations"
				envelope["body"] = common.RawMessage(`{"model":"gpt-4o","prompt":"a red cube","n":1}`)
				one := quote(token.Key)
				require.Equal(t, http.StatusOK, one.Code, one.Body.String())
				envelope["body"] = common.RawMessage(`{"model":"gpt-4o","prompt":"a red cube","n":4}`)
				four := quote(token.Key)
				require.Equal(t, http.StatusOK, four.Code, four.Body.String())
				var oneResult, fourResult map[string]any
				require.NoError(t, common.Unmarshal(one.Body.Bytes(), &oneResult))
				require.NoError(t, common.Unmarshal(four.Body.Bytes(), &fourResult))
				assert.Equal(t, tc.oneQuota, oneResult["quota"])
				assert.Equal(t, tc.fourQuota, fourResult["quota"])
				assert.Equal(t, tc.oneAmount, oneResult["amount"])
				assert.Equal(t, tc.fourAmount, fourResult["amount"])
			})
		}
		require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"billing_setting.billing_mode": `{}`, "billing_setting.billing_expr": `{}`}))
		envelope["body"] = common.RawMessage(`{"model":"gpt-4o","prompt":"a red cube","n":1}`)

		originalAbility := model.Ability{}
		require.NoError(t, model.DB.Where("channel_id = ? AND model = ?", channel.Id, "gpt-4o").First(&originalAbility).Error)
		require.True(t, originalAbility.Enabled, "fixture image ability must start enabled")
		t.Cleanup(func() {
			require.NoError(t, model.DB.Model(&model.Ability{}).Where(&model.Ability{ChannelId: channel.Id, Model: "gpt-4o", Group: "default"}).Update("enabled", originalAbility.Enabled).Error)
			model.InitChannelCache()
		})
		require.NoError(t, model.DB.Model(&model.Ability{}).Where(&model.Ability{ChannelId: channel.Id, Model: "gpt-4o", Group: "default"}).Update("enabled", false).Error)
		replicateChannel := model.Channel{Type: constant.ChannelTypeReplicate, Key: "replicate-test-key", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled, Models: "black-forest-labs/flux-1.1-pro", Group: "default"}
		require.NoError(t, model.DB.Create(&replicateChannel).Error)
		t.Cleanup(func() {
			require.NoError(t, model.DB.Where("channel_id = ?", replicateChannel.Id).Delete(&model.Ability{}).Error)
			require.NoError(t, model.DB.Delete(&replicateChannel).Error)
			model.InitChannelCache()
		})
		require.NoError(t, model.DB.Create(&model.Ability{Group: "default", Model: "black-forest-labs/flux-1.1-pro", ChannelId: replicateChannel.Id, Enabled: true}).Error)
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"black-forest-labs/flux-1.1-pro":0.02}`))
		beforeCalls := calls.Load()
		envelope["path"] = "/v1/images/edits"
		form.Reset()
		writer = multipart.NewWriter(&form)
		require.NoError(t, writer.WriteField("model", "black-forest-labs/flux-1.1-pro"))
		require.NoError(t, writer.WriteField("prompt", "edit this image"))
		require.NoError(t, writer.WriteField("n", "2"))
		file, err = writer.CreateFormFile("image", "input.png")
		require.NoError(t, err)
		_, err = file.Write([]byte("image bytes"))
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		delete(envelope, "body")
		envelope["body_base64"] = base64.StdEncoding.EncodeToString(form.Bytes())
		envelope["headers"] = http.Header{"Content-Type": {writer.FormDataContentType()}}
		result = quote(token.Key)
		require.Equal(t, http.StatusOK, result.Code, result.Body.String())
		assert.Contains(t, result.Body.String(), `"quota":20000`)
		assert.Contains(t, result.Body.String(), `"amount":"0.04"`)
		assert.Equal(t, beforeCalls, calls.Load(), "Replicate edit estimates must not upload files or submit predictions")
		require.NoError(t, model.DB.Model(&model.Ability{}).Where(&model.Ability{ChannelId: channel.Id, Model: "gpt-4o", Group: "default"}).Update("enabled", originalAbility.Enabled).Error)
		model.InitChannelCache()
		delete(envelope, "body_base64")
		delete(envelope, "headers")
		envelope["body"] = common.RawMessage(body)
		envelope["method"], envelope["path"], envelope["body"] = http.MethodPost, "/v1/chat/completions", common.RawMessage(body)
	})
	t.Run("plugin-owned Responses model is not priced as a channel request", func(t *testing.T) {
		registry := pluginruntime.NewRegistry()
		_, err := registry.Register(`
			export const meta = {
				apiVersion: 1, key: "estimate-responses-plugin", name: "Estimate responses plugin",
				version: "1.0.0", author: {name: "Test"}, models: ["plugin-responses-model"], fetchMode: "per_task",
				protocols: [{name: "openai_responses", models: ["plugin-responses-model"], supports: ["sync"]}]
			};
			export const protocols = {openai_responses: {
				decodeRequest(request) { return {model: request.body.model, input: request.body.input}; },
				renderFinal() { return {}; }
			}};
			export function buildSubmitRequest() { return {}; }
			export function parseSubmitResponse() { return {taskId: "task-1"}; }
			export function parseTaskResult() { return {status: "processing"}; }
			export function buildQueryRequest() { return {}; }
		`, pluginruntime.Options{Key: "estimate-responses-plugin", Version: "1.0.0"})
		require.NoError(t, err)
		previous := pluginruntime.DefaultRegistry
		pluginruntime.DefaultRegistry = registry
		t.Cleanup(func() { pluginruntime.DefaultRegistry = previous })
		envelope["method"], envelope["path"] = http.MethodPost, "/v1/responses"
		envelope["body"], envelope["headers"] = common.RawMessage(`{"model":"plugin-responses-model","input":"hello"}`), nil
		assert.Equal(t, http.StatusUnprocessableEntity, quote(token.Key).Code)
		envelope["method"], envelope["path"], envelope["body"], envelope["headers"] = http.MethodPost, "/v1/chat/completions", common.RawMessage(body), nil
	})
	t.Run("audio multipart accepts request-scoped duration facts", func(t *testing.T) {
		savedCount, savedRatio := constant.CountToken, ratio_setting.ModelRatio2JSONString()
		constant.CountToken = true
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"estimate-audio":0.01}`))
		t.Cleanup(func() {
			constant.CountToken = savedCount
			require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(savedRatio))
		})
		var multipartBody bytes.Buffer
		writer := multipart.NewWriter(&multipartBody)
		require.NoError(t, writer.WriteField("model", "estimate-audio"))
		file, err := writer.CreateFormFile("file", "audio.wav")
		require.NoError(t, err)
		_, err = file.Write([]byte("audio bytes are not parsed when duration is supplied"))
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		contentType := writer.FormDataContentType()
		envelope["method"], envelope["path"] = http.MethodPost, "/v1/audio/transcriptions"
		envelope["headers"] = http.Header{"Content-Type": {contentType}}
		delete(envelope, "body")
		envelope["body_base64"] = base64.StdEncoding.EncodeToString(multipartBody.Bytes())
		envelope["media_info"] = []map[string]any{{"field": "file", "index": 0, "type": "audio", "duration_seconds": 60}}
		w := quote(token.Key)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var result map[string]any
		require.NoError(t, common.Unmarshal(w.Body.Bytes(), &result))
		assert.Equal(t, "estimate-audio", result["model"])
		assert.Equal(t, float64(10), result["quota"])
		assert.Equal(t, "0.00002", result["amount"])
		delete(envelope, "body_base64")
		delete(envelope, "media_info")
		envelope["method"], envelope["path"], envelope["body"], envelope["headers"] = http.MethodPost, "/v1/chat/completions", common.RawMessage(body), nil
	})
	t.Run("multimodal image dimensions bind to the original JSON pointer", func(t *testing.T) {
		savedCount, savedRatio := constant.CountToken, ratio_setting.ModelRatio2JSONString()
		savedMediaToken, savedMediaTokenNotStream := constant.GetMediaToken, constant.GetMediaTokenNotStream
		constant.CountToken = true
		constant.GetMediaToken, constant.GetMediaTokenNotStream = true, true
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-4o":0.01}`))
		t.Cleanup(func() {
			constant.CountToken = savedCount
			constant.GetMediaToken, constant.GetMediaTokenNotStream = savedMediaToken, savedMediaTokenNotStream
			require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(savedRatio))
		})
		imageBody := `{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,not-decoded","detail":"high"}}]}]}`
		quoteImage := func(width, height int) int {
			envelope["method"], envelope["path"], envelope["body"] = http.MethodPost, "/v1/chat/completions", common.RawMessage(imageBody)
			envelope["headers"] = http.Header{"Content-Type": {"application/json"}}
			envelope["media_info"] = []map[string]any{{"pointer": "/messages/0/content/0/image_url/url", "type": "image", "width": width, "height": height}}
			w := quote(token.Key)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var result map[string]any
			require.NoError(t, common.Unmarshal(w.Body.Bytes(), &result))
			return int(result["quota"].(float64))
		}
		low := quoteImage(512, 512)
		high := quoteImage(2048, 2048)
		assert.Equal(t, 2, low)
		assert.Equal(t, 7, high)
		assert.Greater(t, high, low, "the bound image dimensions must affect existing image token estimation")
		delete(envelope, "media_info")
		envelope["method"], envelope["path"], envelope["body"], envelope["headers"] = http.MethodPost, "/v1/chat/completions", common.RawMessage(body), nil
	})
	t.Run("image URL measurements take precedence and report media failures", func(t *testing.T) {
		savedCount, savedRatio := constant.CountToken, ratio_setting.ModelRatio2JSONString()
		savedMaxFileDownloadMB := constant.MaxFileDownloadMB
		savedMediaToken, savedMediaTokenNotStream := constant.GetMediaToken, constant.GetMediaTokenNotStream
		fetchSetting := system_setting.GetFetchSetting()
		savedFetch := *fetchSetting
		constant.CountToken = true
		constant.MaxFileDownloadMB = 1
		constant.GetMediaToken, constant.GetMediaTokenNotStream = true, true
		fetchSetting.EnableSSRFProtection = true
		fetchSetting.AllowPrivateIp = true
		fetchSetting.DomainFilterMode, fetchSetting.IpFilterMode = false, false
		fetchSetting.DomainList, fetchSetting.IpList, fetchSetting.AllowedPorts = nil, nil, nil
		fetchSetting.ApplyIPFilterForDomain = true
		service.InitHttpClient()
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-4o":0.01}`))
		t.Cleanup(func() {
			constant.CountToken = savedCount
			constant.MaxFileDownloadMB = savedMaxFileDownloadMB
			constant.GetMediaToken, constant.GetMediaTokenNotStream = savedMediaToken, savedMediaTokenNotStream
			*fetchSetting = savedFetch
			require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(savedRatio))
		})
		pngBytes := func() []byte {
			var data bytes.Buffer
			img := image.NewRGBA(image.Rect(0, 0, 1, 1))
			img.Set(0, 0, color.White)
			require.NoError(t, png.Encode(&data, img))
			return data.Bytes()
		}()
		var calls atomic.Int32
		mediaServerFails := false
		mediaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if mediaServerFails {
				http.Error(w, "media unavailable", http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes)
		}))
		t.Cleanup(mediaServer.Close)
		imageBody := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":%q,"detail":"high"}}]}]}`, mediaServer.URL+"/image.png")
		quoteImage := func(withFacts bool) int {
			envelope["method"], envelope["path"], envelope["body"] = http.MethodPost, "/v1/chat/completions", common.RawMessage(imageBody)
			envelope["headers"] = http.Header{"Content-Type": {"application/json"}}
			if withFacts {
				envelope["media_info"] = []map[string]any{{"pointer": "/messages/0/content/0/image_url/url", "type": "image", "width": 4096, "height": 4096}}
			} else {
				delete(envelope, "media_info")
			}
			w := quote(token.Key)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var result map[string]any
			require.NoError(t, common.Unmarshal(w.Body.Bytes(), &result))
			return int(result["quota"].(float64))
		}
		withFacts := quoteImage(true)
		withoutFacts := quoteImage(false)
		assert.Equal(t, 2, withFacts)
		assert.Equal(t, 2, withoutFacts)
		assert.Equal(t, withoutFacts, withFacts, "URL dimensions, not conflicting supplements, determine the measured quote")
		assert.EqualValues(t, 2, calls.Load())
		mediaServerFails = true
		envelope["method"], envelope["path"], envelope["body"] = http.MethodPost, "/v1/chat/completions", common.RawMessage(imageBody)
		envelope["headers"] = http.Header{"Content-Type": {"application/json"}}
		envelope["media_info"] = []map[string]any{{"pointer": "/messages/0/content/0/image_url/url", "type": "image", "width": 1, "height": 1}}
		failedQuote := quote(token.Key)
		assert.Equal(t, http.StatusBadRequest, failedQuote.Code, failedQuote.Body.String())
		assert.Contains(t, failedQuote.Body.String(), "media_read_failed")
		assert.EqualValues(t, 3, calls.Load(), "a failed URL read must not fall back to the supplied dimensions")

		previousWorkerURL, previousWorkerKey := system_setting.WorkerUrl, system_setting.WorkerValidKey
		previousAllowWorkerHTTP := system_setting.WorkerAllowHttpImageRequestEnabled
		t.Cleanup(func() {
			system_setting.WorkerUrl, system_setting.WorkerValidKey = previousWorkerURL, previousWorkerKey
			system_setting.WorkerAllowHttpImageRequestEnabled = previousAllowWorkerHTTP
		})
		var workerCalls atomic.Int32
		worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			workerCalls.Add(1)
			var request service.WorkerRequest
			require.NoError(t, common.DecodeJson(r.Body, &request))
			assert.Contains(t, request.URL, "http://127.0.0.1:8123/")
			assert.Equal(t, "estimate-worker-key", request.Key)
			if request.URL == "http://127.0.0.1:8123/slow.png" {
				<-r.Context().Done()
				return
			}
			switch request.URL {
			case "http://127.0.0.1:8123/unavailable.png":
				http.Error(w, "media unavailable", http.StatusBadGateway)
			case "http://127.0.0.1:8123/empty.png":
			case "http://127.0.0.1:8123/invalid.png":
				_, _ = w.Write([]byte("not an image"))
			case "http://127.0.0.1:8123/oversized.png":
				_, _ = w.Write(bytes.Repeat([]byte{'x'}, (1<<20)+1))
			default:
				_, _ = w.Write(pngBytes)
			}
		}))
		t.Cleanup(worker.Close)
		system_setting.WorkerUrl = worker.URL
		system_setting.WorkerValidKey = "estimate-worker-key"
		system_setting.WorkerAllowHttpImageRequestEnabled = true
		fetchSetting.EnableSSRFProtection = false
		mediaServerFails = false
		imageBody = `{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://127.0.0.1:8123/image.png","detail":"high"}}]}]}`
		envelope["method"], envelope["path"], envelope["body"] = http.MethodPost, "/v1/chat/completions", common.RawMessage(imageBody)
		envelope["headers"] = http.Header{"Content-Type": {"application/json"}}
		envelope["media_info"] = []map[string]any{{"pointer": "/messages/0/content/0/image_url/url", "type": "image", "width": 4096, "height": 4096}}
		workerQuote := quote(token.Key)
		require.Equal(t, http.StatusOK, workerQuote.Code, workerQuote.Body.String())
		var workerResult map[string]any
		require.NoError(t, common.Unmarshal(workerQuote.Body.Bytes(), &workerResult))
		assert.Equal(t, float64(2), workerResult["quota"])
		assert.Equal(t, "0.000004", workerResult["amount"])
		assert.EqualValues(t, 1, workerCalls.Load())
		assert.EqualValues(t, 3, calls.Load(), "Worker-backed estimation must not fetch the media URL directly")

		for _, path := range []string{"unavailable.png", "empty.png", "invalid.png", "oversized.png"} {
			imageBody = fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":%q,"detail":"high"}}]}]}`, "http://127.0.0.1:8123/"+path)
			envelope["body"] = common.RawMessage(imageBody)
			failedWorkerQuote := quote(token.Key)
			assert.Equal(t, http.StatusBadRequest, failedWorkerQuote.Code, failedWorkerQuote.Body.String())
			assert.Contains(t, failedWorkerQuote.Body.String(), "media_read_failed")
		}
		assert.EqualValues(t, 5, workerCalls.Load())
		assert.EqualValues(t, 3, calls.Load(), "Worker media failures must not fetch the media URL directly")

		imageBody = `{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://127.0.0.1:8123/slow.png","detail":"high"}}]}]}`
		envelope["body"] = common.RawMessage(imageBody)
		envelope["media_info"] = []map[string]any{{"pointer": "/messages/0/content/0/image_url/url", "type": "image", "width": 1, "height": 1}}
		requestContext, cancelRequest := context.WithTimeout(context.Background(), 500*time.Millisecond)
		timeoutQuote := quoteWithContext(token.Key, requestContext)
		cancelRequest()
		assert.Equal(t, http.StatusGatewayTimeout, timeoutQuote.Code, timeoutQuote.Body.String())
		assert.Contains(t, timeoutQuote.Body.String(), "media_read_failed")
		assert.EqualValues(t, 6, workerCalls.Load())
		assert.EqualValues(t, 3, calls.Load(), "a timed-out URL read must not fall back to supplied dimensions")

		delete(envelope, "media_info")
		envelope["method"], envelope["path"], envelope["body"], envelope["headers"] = http.MethodPost, "/v1/chat/completions", common.RawMessage(body), nil
	})
	t.Run("audio speech and translation routes are estimable", func(t *testing.T) {
		savedRatio := ratio_setting.ModelRatio2JSONString()
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"estimate-audio":0.01}`))
		t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(savedRatio)) })
		envelope["method"], envelope["path"] = http.MethodPost, "/v1/audio/speech"
		envelope["headers"], envelope["body"] = http.Header{"Content-Type": {"application/json"}}, common.RawMessage(`{"model":"estimate-audio","input":"hello","voice":"alloy"}`)
		assert.Equal(t, http.StatusOK, quote(token.Key).Code)

		var multipartBody bytes.Buffer
		writer := multipart.NewWriter(&multipartBody)
		require.NoError(t, writer.WriteField("model", "estimate-audio"))
		file, err := writer.CreateFormFile("file", "audio.wav")
		require.NoError(t, err)
		_, err = file.Write([]byte("audio bytes"))
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		envelope["path"], envelope["headers"] = "/v1/audio/translations", http.Header{"Content-Type": {writer.FormDataContentType()}}
		envelope["body_base64"] = base64.StdEncoding.EncodeToString(multipartBody.Bytes())
		delete(envelope, "body")
		assert.Equal(t, http.StatusOK, quote(token.Key).Code)
		delete(envelope, "body_base64")
		envelope["method"], envelope["path"], envelope["body"], envelope["headers"] = http.MethodPost, "/v1/chat/completions", common.RawMessage(body), nil
	})
	t.Run("malformed or unbound media facts are rejected", func(t *testing.T) {
		envelope["method"], envelope["path"], envelope["body"] = http.MethodPost, "/v1/chat/completions", common.RawMessage(body)
		for _, facts := range [][]map[string]any{
			{{"pointer": "/messages/0/content/0/text", "type": "image", "width": 128, "height": 128, "quota": 1}},
			{{"pointer": "/messages/0/content/0/text", "type": "audio", "duration_seconds": -1}},
			{{"pointer": "/messages/0/content/0/text", "type": "image", "width": 32768, "height": 32768}},
			{{"pointer": "/messages/0/content/99/text", "type": "image", "width": 128, "height": 128}},
			{{"pointer": "/messages/0/content/0/text", "field": "file", "index": 0, "type": "image", "width": 128, "height": 128}},
			{{"pointer": "/messages/0/content/0/text", "type": "image", "width": 128, "height": 128}, {"pointer": "/messages/0/content/0/text", "type": "image", "width": 256, "height": 256}},
		} {
			envelope["media_info"] = facts
			assert.Equal(t, http.StatusBadRequest, quote(token.Key).Code)
		}
		delete(envelope, "media_info")
		delete(envelope, "body_base64")
		envelope["method"], envelope["path"], envelope["body"], envelope["headers"] = http.MethodPost, "/v1/chat/completions", common.RawMessage(body), nil
	})
	t.Run("nominal prices ignore funding optimizations", func(t *testing.T) {
		operation_setting.GetQuotaSetting().PreConsumeMultiplier = 3
		// "user\nhello" estimates three text tokens plus six chat framing
		// tokens. At ratio 2 and multiplier 3 this reserves 54 quota; the
		// expression's $4/M input price gives the same independent example.
		for _, tc := range []struct {
			name, price, ratio, expr string
			count                    bool
			quota                    int
			amount                   string
			status                   int
		}{
			{"per request", `{"estimate-chat":0.01}`, `{}`, "", false, 5000, "0.01", 200},
			{"free", `{"estimate-chat":0}`, `{}`, "", false, 0, "0", 200},
			{"overflow", `{"estimate-chat":1e100}`, `{}`, "", false, 0, "", 400},
			{"negative", `{"estimate-chat":-1}`, `{}`, "", false, 0, "", 400},
			{"missing", `{}`, `{}`, "", false, 0, "", 400},
			{"legacy tokens", `{}`, `{"estimate-chat":2}`, "", true, 54, "0.000108", 200},
			{"token expression", `{}`, `{}`, `tier("text", p * 4 + c * 999)`, true, 54, "0.000108", 200},
			{"fixed expression", `{}`, `{}`, `tier("request", fixed(0.02))`, false, 10000, "0.02", 200},
			{"zero expression", `{}`, `{}`, `tier("request", fixed(0))`, false, 0, "0", 200},
			{"raw body and header", `{}`, `{}`, `param("max_tokens") == 0 && header("x-price-tier") == "fast" ? tier("fast", fixed(0.03)) : tier("base", fixed(0.01))`, false, 15000, "0.03", 200},
		} {
			t.Run(tc.name, func(t *testing.T) {
				constant.CountToken = tc.count
				require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(tc.price))
				require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(tc.ratio))
				mode := "ratio"
				if tc.expr != "" {
					mode = "tiered_expr"
				}
				modeJSON, err := common.Marshal(map[string]string{"estimate-chat": mode})
				require.NoError(t, err)
				exprJSON, err := common.Marshal(map[string]string{"estimate-chat": tc.expr})
				require.NoError(t, err)
				require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"billing_setting.billing_mode": string(modeJSON), "billing_setting.billing_expr": string(exprJSON)}))
				envelope["headers"] = http.Header{"X-Price-Tier": {"fast", "slow"}}
				w := quote(token.Key)
				require.Equal(t, tc.status, w.Code, w.Body.String())
				if tc.status == 200 {
					var result map[string]any
					require.NoError(t, common.Unmarshal(w.Body.Bytes(), &result))
					assert.Equal(t, float64(tc.quota), result["quota"])
					assert.Equal(t, tc.amount, result["amount"])
				}
			})
		}
		constant.CountToken = false
	})
	t.Run("invalid conversion unit", func(t *testing.T) {
		savedUnit := common.QuotaPerUnit
		defer func() { common.QuotaPerUnit = savedUnit }()
		for _, unit := range []float64{-1, math.NaN(), math.Inf(1)} {
			common.QuotaPerUnit = unit
			assert.Equal(t, 400, quote(token.Key).Code)
		}
	})
	t.Run("funding does not change nominal price", func(t *testing.T) {
		require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"billing_setting.billing_mode": `{}`, "billing_setting.billing_expr": `{}`}))
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"estimate-chat":0.01}`))
		for _, preference := range []string{"wallet_only", "subscription_only"} {
			user.Setting = fmt.Sprintf(`{"billing_preference":%q}`, preference)
			user.Quota, token.RemainQuota = 100_000_000, 100_000_000
			require.NoError(t, model.DB.Save(&user).Error)
			require.NoError(t, model.DB.Save(&token).Error)
			w := quote(token.Key)
			require.Equal(t, 200, w.Code, w.Body.String())
			assert.JSONEq(t, fmt.Sprintf(`{"quota":5000,"amount":"0.01","currency":"USD","model":"estimate-chat","group":"default","channel":{"id":%d,"type":1}}`, channel.Id), w.Body.String())
		}
		user.Quota, token.RemainQuota = 0, 0
		require.NoError(t, model.DB.Save(&user).Error)
		require.NoError(t, model.DB.Save(&token).Error)
	})
	t.Run("estimate and model request limits are independent", func(t *testing.T) {
		savedEnabled, savedCount, savedSuccess, savedDuration := setting.ModelRequestRateLimitEnabled, setting.ModelRequestRateLimitCount, setting.ModelRequestRateLimitSuccessCount, setting.ModelRequestRateLimitDurationMinutes
		defer func() {
			setting.ModelRequestRateLimitEnabled, setting.ModelRequestRateLimitCount, setting.ModelRequestRateLimitSuccessCount, setting.ModelRequestRateLimitDurationMinutes = savedEnabled, savedCount, savedSuccess, savedDuration
		}()
		setting.ModelRequestRateLimitEnabled, setting.ModelRequestRateLimitCount, setting.ModelRequestRateLimitSuccessCount, setting.ModelRequestRateLimitDurationMinutes = true, 1, 1, 1
		// A test-only HTTP probe observes the real model limiter without funding
		// or upstream calls. Estimates must neither consume nor obey its bucket.
		engine.GET("/estimate-test/model-limit", func(c *gin.Context) { c.Set("id", user.Id) }, middleware.ModelRequestRateLimit(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
		assert.Equal(t, 200, quote(token.Key).Code)
		assert.Equal(t, 200, quote(token.Key).Code)
		probe := func() int {
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/estimate-test/model-limit", nil))
			return w.Code
		}
		assert.Equal(t, http.StatusNoContent, probe())
		assert.Equal(t, http.StatusTooManyRequests, probe())
		assert.Equal(t, 200, quote(token.Key).Code)

		savedCriticalEnabled, savedCriticalNum, savedCriticalDuration := common.CriticalRateLimitEnable, common.CriticalRateLimitNum, common.CriticalRateLimitDuration
		defer func() {
			common.CriticalRateLimitEnable, common.CriticalRateLimitNum, common.CriticalRateLimitDuration = savedCriticalEnabled, savedCriticalNum, savedCriticalDuration
		}()
		common.CriticalRateLimitEnable, common.CriticalRateLimitNum, common.CriticalRateLimitDuration = true, 1, 60
		limited := gin.New()
		require.NoError(t, limited.SetTrustedProxies(nil))
		SetRelayRouter(limited)
		limited.GET("/estimate-test/critical-limit", middleware.CriticalRateLimit(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
		data, err := common.Marshal(envelope)
		require.NoError(t, err)
		for _, status := range []int{200, 429} {
			r := httptest.NewRequest(http.MethodPost, "/v1/requests/estimate", strings.NewReader(string(data)))
			r.Header.Set("Authorization", "Bearer "+token.Key)
			r.Header.Set("Content-Type", "application/json")
			r.RemoteAddr = "192.0.2.99:1234"
			w := httptest.NewRecorder()
			limited.ServeHTTP(w, r)
			assert.Equal(t, status, w.Code, w.Body.String())
			assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		}
		r := httptest.NewRequest(http.MethodGet, "/estimate-test/critical-limit", nil)
		r.RemoteAddr = "192.0.2.99:1234"
		w := httptest.NewRecorder()
		limited.ServeHTTP(w, r)
		assert.Equal(t, http.StatusNoContent, w.Code, "estimate abuse bucket must not consume the shared critical bucket")
	})
	t.Run("channel configuration changes apply", func(t *testing.T) {
		require.NoError(t, model.DB.Model(&model.Ability{}).Where("channel_id = ?", channel.Id).Update("enabled", false).Error)
		assert.Equal(t, 503, quote(token.Key).Code)
		require.NoError(t, model.DB.Model(&model.Ability{}).Where("channel_id = ?", channel.Id).Update("enabled", true).Error)
		assert.Equal(t, 200, quote(token.Key).Code)
	})
	t.Run("auto group and billing alias", func(t *testing.T) {
		savedGroups := setting.UserUsableGroups2JSONString()
		savedGroupRatio := ratio_setting.GroupRatio2JSONString()
		defer func() {
			require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(savedGroups))
			require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(savedGroupRatio))
		}()
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default","vip":"VIP","auto":"Auto"}`))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"vip":2}`))
		token.Group, token.AutoGroups = "auto", `["default","vip"]`
		require.NoError(t, model.DB.Save(&token).Error)
		require.NoError(t, model.DB.Model(&model.Ability{}).Where("channel_id = ?", channel.Id).Update("group", "vip").Error)
		alias := "estimate-chat@effort:high"
		require.NoError(t, model.DB.Create(&model.Ability{Group: "vip", Model: alias, ChannelId: channel.Id, Enabled: true}).Error)
		for _, expr := range []string{"", `tier("request", fixed(0.02))`} {
			mode := "ratio"
			quota, amount := 10000, "0.02"
			if expr != "" {
				mode, quota, amount = "tiered_expr", 20000, "0.04"
			}
			expressions, err := common.Marshal(map[string]string{"estimate-chat": expr})
			require.NoError(t, err)
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"billing_setting.billing_mode": fmt.Sprintf(`{"estimate-chat":%q}`, mode), "billing_setting.billing_expr": string(expressions)}))
			envelope["body"] = common.RawMessage(strings.Replace(body, "estimate-chat", alias, 1))
			w := quote(token.Key)
			require.Equal(t, 200, w.Code, w.Body.String())
			assert.JSONEq(t, fmt.Sprintf(`{"quota":%d,"amount":%q,"currency":"USD","model":"estimate-chat","group":"vip","channel":{"id":%d,"type":1}}`, quota, amount, channel.Id), w.Body.String())
		}
		envelope["body"] = common.RawMessage(body)
		token.Group, token.AutoGroups = "", ""
		require.NoError(t, model.DB.Save(&token).Error)
		require.NoError(t, model.DB.Model(&model.Ability{}).Where("channel_id = ?", channel.Id).Update("group", "default").Error)
		require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"billing_setting.billing_mode": `{}`, "billing_setting.billing_expr": `{}`}))
	})
	t.Run("priority pin cache and affinity remain read only", func(t *testing.T) {
		common.MemoryCacheEnabled = true
		defer func() { common.MemoryCacheEnabled = false }()
		newChannel := model.Channel{Type: constant.ChannelTypeOpenAI, Key: "never-read-secret", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled, Models: "estimate-chat", Group: "default", Priority: new(int64(10))}
		require.NoError(t, model.DB.Create(&newChannel).Error)
		require.NoError(t, model.DB.Create(&model.Ability{Group: "default", Model: "estimate-chat", ChannelId: newChannel.Id, Enabled: true, Priority: newChannel.Priority}).Error)
		model.InitChannelCache()
		selectedID := func(w *httptest.ResponseRecorder) int {
			require.Equal(t, 200, w.Code, w.Body.String())
			var result struct {
				Channel struct {
					ID int `json:"id"`
				} `json:"channel"`
			}
			require.NoError(t, common.Unmarshal(w.Body.Bytes(), &result))
			return result.Channel.ID
		}
		assert.Equal(t, newChannel.Id, selectedID(quote(token.Key)), "new higher-priority configuration takes effect after normal cache publication")
		user.Role = common.RoleAdminUser
		require.NoError(t, model.DB.Save(&user).Error)
		assert.Equal(t, channel.Id, selectedID(quote(fmt.Sprintf("%s-%d", token.Key, channel.Id))))
		user.Role = common.RoleCommonUser
		require.NoError(t, model.DB.Save(&user).Error)
		affinity := operation_setting.GetChannelAffinitySetting()
		savedAffinity := *affinity
		defer func() {
			_, err := service.ClearChannelAffinityCacheByRuleName("estimate01")
			require.NoError(t, err)
			*affinity = savedAffinity
		}()
		affinity.Enabled = true
		affinity.KeepOnChannelDisabled = false
		affinity.Rules = []operation_setting.ChannelAffinityRule{{Name: "estimate01", IncludeRuleName: true, ModelRegex: []string{"^estimate-chat$"}, KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "request_header", Key: "X-Estimate-Session"}}, SessionMode: "prefer"}}
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		ctx.Request.Header.Set("X-Estimate-Session", "existing")
		_, _ = service.GetPreferredChannelByAffinity(ctx, "estimate-chat", "default")
		service.RecordChannelAffinity(ctx, channel.Id)
		beforeAffinity := service.GetChannelAffinityCacheStats()
		envelope["headers"] = http.Header{"X-Estimate-Session": {"existing"}}
		assert.Equal(t, channel.Id, selectedID(quote(token.Key)), "affinity precedes priority")
		cached, err := model.CacheGetChannel(channel.Id)
		require.NoError(t, err)
		assert.Zero(t, cached.ChannelInfo.MultiKeyPollingIndex)
		require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("status", common.ChannelStatusManuallyDisabled).Error)
		model.InitChannelCache()
		assert.Equal(t, newChannel.Id, selectedID(quote(token.Key)), "stale affinity falls back without deletion")
		preferred, found := service.GetPreferredChannelByAffinity(ctx, "estimate-chat", "default")
		assert.True(t, found)
		assert.Equal(t, channel.Id, preferred)
		envelope["headers"] = http.Header{"X-Estimate-Session": {"new"}}
		assert.Equal(t, newChannel.Id, selectedID(quote(token.Key)))
		assert.Equal(t, beforeAffinity, service.GetChannelAffinityCacheStats(), "no new affinity on success or removal on stale binding")
		require.NoError(t, model.DB.Delete(&model.Channel{}, newChannel.Id).Error)
		require.NoError(t, model.DB.Where("channel_id = ?", newChannel.Id).Delete(&model.Ability{}).Error)
		model.InitChannelCache()
		assert.Equal(t, 503, quote(token.Key).Code, "deleted channel disappears after cache publication")
	})
}
