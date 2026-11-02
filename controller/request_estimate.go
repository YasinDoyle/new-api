package controller

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// RequestEstimate stops at nominal pricing and never starts an upstream attempt.
func RequestEstimate(c *gin.Context, relayFormat types.RelayFormat) {
	request, err := helper.GetAndValidateRequest(c, relayFormat)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": i18n.T(c, i18n.MsgEstimateInvalidRequest), "type": "invalid_request_error", "code": "invalid_request"}})
		return
	}
	if _, hasMediaInfo := c.Get(common.RequestEstimateMediaInfoContextKey); hasMediaInfo {
		if err = service.BindRequestEstimateMediaInfo(c, request.GetTokenCountMeta().Files); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": i18n.T(c, i18n.MsgEstimateInvalidRequest), "type": "invalid_request_error", "code": "invalid_request"}})
			return
		}
	}
	info, err := relaycommon.GenRelayInfo(c, relayFormat, request, nil)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": i18n.T(c, i18n.MsgEstimatePricingFailed), "type": "invalid_request_error", "code": "estimate_pricing_failed"}})
		return
	}
	if apiErr := relay.PrepareRequestPricing(c, info); apiErr != nil {
		if apiErr.GetErrorCode() == types.ErrorCodeCountTokenFailed {
			status := http.StatusBadRequest
			var timeoutError net.Error
			if errors.Is(apiErr, context.DeadlineExceeded) || (errors.As(apiErr, &timeoutError) && timeoutError.Timeout()) {
				status = http.StatusGatewayTimeout
			}
			c.JSON(status, gin.H{"error": gin.H{"message": i18n.T(c, i18n.MsgEstimateMediaFailed), "type": "invalid_request_error", "code": "media_read_failed"}})
			return
		}
		c.JSON(apiErr.StatusCode, gin.H{"error": gin.H{"message": i18n.T(c, i18n.MsgEstimatePricingFailed), "type": "invalid_request_error", "code": "estimate_pricing_failed"}})
		return
	}
	quota := info.PriceData.QuotaToPreConsume
	snapshot, refreshErr := service.RefreshTieredBillingGroup(info)
	if snapshot != nil {
		quota = snapshot.EstimatedQuotaAfterGroup
	}
	if refreshErr != nil || quota < 0 || info.QuotaClamp != nil || common.QuotaPerUnit <= 0 || math.IsNaN(common.QuotaPerUnit) || math.IsInf(common.QuotaPerUnit, 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": i18n.T(c, i18n.MsgEstimatePricingFailed), "type": "invalid_request_error", "code": "estimate_pricing_failed"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"quota":    quota,
		"amount":   decimal.NewFromInt(int64(quota)).Div(decimal.NewFromFloat(common.QuotaPerUnit)).String(),
		"currency": "USD", "model": info.GetBillingModelName(), "group": info.UsingGroup,
		"channel": gin.H{"id": c.GetInt("channel_id"), "type": c.GetInt("channel_type")},
	})
}
