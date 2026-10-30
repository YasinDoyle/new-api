package controller

import (
	"math"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// RequestEstimate stops at nominal pricing and never starts an upstream attempt.
func RequestEstimate(c *gin.Context) {
	request, err := helper.GetAndValidateRequest(c, types.RelayFormatOpenAI)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": i18n.T(c, i18n.MsgEstimateInvalidRequest), "type": "invalid_request_error", "code": "invalid_request"}})
		return
	}
	chat := request.(*dto.GeneralOpenAIRequest)
	unsupported := chat.IsStream(c.Request)
	for _, message := range chat.Messages {
		for _, part := range message.ParseContent() {
			if part.Type != dto.ContentTypeText {
				unsupported = true
			}
		}
	}
	if unsupported {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": gin.H{"message": i18n.T(c, i18n.MsgEstimateUnsupported), "type": "invalid_request_error", "code": "estimate_unsupported"}})
		return
	}
	info, err := relaycommon.GenRelayInfo(c, types.RelayFormatOpenAI, request, nil)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": i18n.T(c, i18n.MsgEstimatePricingFailed), "type": "invalid_request_error", "code": "estimate_pricing_failed"}})
		return
	}
	if apiErr := relay.PrepareRequestPricing(c, info); apiErr != nil {
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
