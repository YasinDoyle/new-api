package router

import (
	"encoding/base64"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
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
	channel := model.Channel{Type: constant.ChannelTypeOpenAI, Key: "secret-one\nsecret-two", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled, Models: "estimate-chat", Group: "default", ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}}
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, model.DB.Create(&model.Ability{Group: "default", Model: "estimate-chat", ChannelId: channel.Id, Enabled: true}).Error)
	engine := gin.New()
	require.NoError(t, engine.SetTrustedProxies(nil))
	SetRelayRouter(engine)
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
	quote := func(auth string) *httptest.ResponseRecorder {
		data, err := common.Marshal(envelope)
		require.NoError(t, err)
		before := snapshot()
		r := httptest.NewRequest(http.MethodPost, "/v1/requests/estimate", strings.NewReader(string(data)))
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
		assert.Equal(t, 422, quote(token.Key).Code)
		envelope["body"] = common.RawMessage(strings.Replace(body, `"max_tokens":0`, `"max_tokens":18446744073709551615`, 1))
		assert.Equal(t, 400, quote(token.Key).Code)
		envelope["body"] = common.RawMessage(body)
		envelope["headers"] = http.Header{"Content-Type": {"application/json-invalid"}}
		assert.Equal(t, 422, quote(token.Key).Code, "invalid media type must not be accepted as JSON")
		delete(envelope, "headers")
		envelope["body"] = common.RawMessage(`{"model":"estimate-chat","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + upstream.URL + `/image"}}]}]}`)
		assert.Equal(t, 422, quote(token.Key).Code, "multimodal is deferred, including media downloads")
		envelope["body"] = common.RawMessage(body)
		savedMaxBody := constant.MaxRequestBodyMB
		constant.MaxRequestBodyMB = 1
		envelope["padding"] = strings.Repeat("x", 1<<20)
		assert.Equal(t, 413, quote(token.Key).Code)
		delete(envelope, "padding")
		constant.MaxRequestBodyMB = savedMaxBody
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
