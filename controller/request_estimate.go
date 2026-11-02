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
func RequestEstimate(c *gin.Context, relayFormat types.RelayFormat) {
	request, err := helper.GetAndValidateRequest(c, relayFormat)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": i18n.T(c, i18n.MsgEstimateInvalidRequest), "type": "invalid_request_error", "code": "invalid_request"}})
		return
	}
	unsupported := false
	if relayFormat == types.RelayFormatOpenAI {
		chat, ok := request.(*dto.GeneralOpenAIRequest)
		if !ok {
			unsupported = true
		} else {
			for _, message := range chat.Messages {
				for _, part := range message.ParseContent() {
					if part.Type != dto.ContentTypeText {
						unsupported = true
					}
				}
			}
		}
	}
	if meta := request.GetTokenCountMeta(); meta != nil && len(meta.Files) > 0 {
		unsupported = true
	}
	switch request := request.(type) {
	case *dto.GeminiChatRequest:
		for _, content := range request.Contents {
			for _, part := range content.Parts {
				if part.InlineData != nil || part.FileData != nil {
					unsupported = true
				}
			}
		}
	case *dto.GeminiEmbeddingRequest:
		for _, part := range request.Content.Parts {
			if part.InlineData != nil || part.FileData != nil {
				unsupported = true
			}
		}
	case *dto.GeminiBatchEmbeddingRequest:
		for _, item := range request.Requests {
			for _, part := range item.Content.Parts {
				if part.InlineData != nil || part.FileData != nil {
					unsupported = true
				}
			}
		}
	}
	if unsupported {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": gin.H{"message": i18n.T(c, i18n.MsgEstimateUnsupported), "type": "invalid_request_error", "code": "estimate_unsupported"}})
		return
	}
	info, err := relaycommon.GenRelayInfo(c, relayFormat, request, nil)
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
