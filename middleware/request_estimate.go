package middleware

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/gin-gonic/gin"
	"golang.org/x/net/http/httpguts"
)

// EstimateTarget restores a target request after the outer credential has been checked.
// targetPath comes from the same declaration that registers the real relay route.
func EstimateTarget(targetPath string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		var envelope struct {
			Method     string          `json:"method"`
			Path       string          `json:"path"`
			Headers    http.Header     `json:"headers"`
			Body       json.RawMessage `json:"body"`
			BodyBase64 *string         `json:"body_base64"`
			MediaInfo  json.RawMessage `json:"media_info"`
		}
		if err := common.UnmarshalBodyReusable(c, &envelope); err != nil {
			status := http.StatusBadRequest
			if common.IsRequestBodyTooLargeError(err) {
				status = http.StatusRequestEntityTooLarge
			}
			abortWithOpenAiMessage(c, status, i18n.T(c, i18n.MsgEstimateInvalidRequest))
			return
		}
		target, err := url.ParseRequestURI(envelope.Path)
		if err != nil || target.IsAbs() || target.Host != "" || strings.HasPrefix(envelope.Path, "//") || strings.Contains(envelope.Path, "#") || path.Clean(target.Path) != target.Path || strings.ContainsAny(target.Path, "\\\x00") {
			abortWithOpenAiMessage(c, http.StatusBadRequest, i18n.T(c, i18n.MsgEstimateInvalidRequest))
			return
		}
		query, err := url.ParseQuery(target.RawQuery)
		if err != nil || (len(envelope.Body) > 0 && envelope.BodyBase64 != nil) {
			abortWithOpenAiMessage(c, http.StatusBadRequest, i18n.T(c, i18n.MsgEstimateInvalidRequest))
			return
		}
		if envelope.Method != http.MethodPost || target.Path != targetPath || len(envelope.MediaInfo) > 0 {
			abortWithOpenAiMessage(c, http.StatusUnprocessableEntity, i18n.T(c, i18n.MsgEstimateUnsupported))
			return
		}
		body := []byte(envelope.Body)
		if envelope.BodyBase64 != nil {
			body, err = base64.StdEncoding.Strict().DecodeString(*envelope.BodyBase64)
			if err != nil {
				abortWithOpenAiMessage(c, http.StatusBadRequest, i18n.T(c, i18n.MsgEstimateInvalidRequest))
				return
			}
		}
		if len(body) == 0 {
			abortWithOpenAiMessage(c, http.StatusBadRequest, i18n.T(c, i18n.MsgEstimateInvalidRequest))
			return
		}
		headers := make(http.Header)
		for name, values := range envelope.Headers {
			if !httpguts.ValidHeaderFieldName(name) {
				abortWithOpenAiMessage(c, http.StatusBadRequest, i18n.T(c, i18n.MsgEstimateInvalidRequest))
				return
			}
			for _, value := range values {
				if !httpguts.ValidHeaderFieldValue(value) {
					abortWithOpenAiMessage(c, http.StatusBadRequest, i18n.T(c, i18n.MsgEstimateInvalidRequest))
					return
				}
			}
			switch strings.ToLower(name) {
			case "authorization", "proxy-authorization", "x-api-key", "x-goog-api-key", "mj-api-secret", "cookie", "host", "forwarded", "x-real-ip", "x-client-ip", "true-client-ip", "cf-connecting-ip", "connection", "upgrade", "content-length", "transfer-encoding", "content-encoding":
				continue
			}
			if strings.HasPrefix(strings.ToLower(name), "x-forwarded-") || strings.HasPrefix(strings.ToLower(name), "sec-websocket-") {
				continue
			}
			for _, value := range values {
				headers.Add(name, value)
			}
		}
		if headers.Get("Content-Type") == "" {
			headers.Set("Content-Type", "application/json")
		}
		mediaType, _, err := mime.ParseMediaType(headers.Get("Content-Type"))
		if err != nil {
			abortWithOpenAiMessage(c, http.StatusBadRequest, i18n.T(c, i18n.MsgEstimateInvalidRequest))
			return
		}
		if mediaType != "application/json" {
			abortWithOpenAiMessage(c, http.StatusUnprocessableEntity, i18n.T(c, i18n.MsgEstimateUnsupported))
			return
		}
		query.Del("key")
		// Keep the original query bytes unless an identity parameter must be removed.
		if _, present := target.Query()["key"]; present {
			target.RawQuery = query.Encode()
		}
		outer := c.Request
		clientIP := c.ClientIP()
		inner := outer.Clone(outer.Context())
		inner.Method, inner.URL, inner.Header = envelope.Method, target, headers
		inner.Host = ""
		inner.RequestURI = target.RequestURI()
		inner.RemoteAddr = net.JoinHostPort(clientIP, "0")
		inner.Body = io.NopCloser(bytes.NewReader(body))
		inner.ContentLength = int64(len(body))
		common.CleanupBodyStorage(c)
		c.Request = inner
		defer func() { common.CleanupBodyStorage(c); c.Request = outer }()
		c.Next()
	}
}
