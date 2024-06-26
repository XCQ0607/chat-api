package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"one-api/common"
	"one-api/common/logger"
	"one-api/model"
	"one-api/relay/channel/openai"
	"one-api/relay/constant"
	"one-api/relay/helper"
	relaymodel "one-api/relay/model"
	"one-api/relay/util"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

func isWithinRange(element string, value int) bool {
	if _, ok := constant.DalleGenerationImageAmounts[element]; !ok {
		return false
	}
	min := constant.DalleGenerationImageAmounts[element][0]
	max := constant.DalleGenerationImageAmounts[element][1]

	return value >= min && value <= max
}

func RelayImageHelper(c *gin.Context, relayMode int) *relaymodel.ErrorWithStatusCode {
	ctx := c.Request.Context()
	meta := util.GetRelayMeta(c)
	channelName := c.GetString("channel_name")
	group := c.GetString("group")
	startTime := time.Now()
	imageRequest, err := getImageRequest(c, meta.Mode)
	if err != nil {
		logger.Errorf(ctx, "getImageRequest failed: %s", err.Error())
		return openai.ErrorWrapper(err, "invalid_image_request", http.StatusBadRequest)
	}

	// map model name
	var isModelMapped bool
	meta.OriginModelName = imageRequest.Model
	imageRequest.Model, isModelMapped = util.GetMappedModelName(imageRequest.Model, meta.ModelMapping)
	meta.ActualModelName = imageRequest.Model

	// model validation
	bizErr := validateImageRequest(imageRequest, meta)
	if bizErr != nil {
		return bizErr
	}

	imageCostRatio, err := getImageCostRatio(imageRequest)
	if err != nil {
		return openai.ErrorWrapper(err, "get_image_cost_ratio_failed", http.StatusInternalServerError)
	}

	var requestBody io.Reader
	if isModelMapped || meta.ChannelType == common.ChannelTypeAzure { // make Azure channel request body
		jsonStr, err := json.Marshal(imageRequest)
		if err != nil {
			return openai.ErrorWrapper(err, "marshal_image_request_failed", http.StatusInternalServerError)
		}
		requestBody = bytes.NewBuffer(jsonStr)
	} else {
		requestBody = c.Request.Body
	}

	adaptor := helper.GetAdaptor(meta.APIType)
	if adaptor == nil {
		return openai.ErrorWrapper(fmt.Errorf("invalid api type: %d", meta.APIType), "invalid_api_type", http.StatusBadRequest)
	}

	switch meta.ChannelType {
	case common.ChannelTypeAli:
		fallthrough
	case common.ChannelTypeBaidu:
		fallthrough
	case common.ChannelTypeZhipu:
		finalRequest, err := adaptor.ConvertImageRequest(imageRequest)
		if err != nil {
			return openai.ErrorWrapper(err, "convert_image_request_failed", http.StatusInternalServerError)
		}

		jsonStr, err := json.Marshal(finalRequest)
		if err != nil {
			return openai.ErrorWrapper(err, "marshal_image_request_failed", http.StatusInternalServerError)
		}
		requestBody = bytes.NewBuffer(jsonStr)
	}

	modelRatio := common.GetModelRatio(imageRequest.Model)
	groupRatio := common.GetGroupRatio(group)
	ratio := modelRatio * groupRatio
	userQuota, err := model.CacheGetUserQuota(c, meta.UserId)
	sizeRatio := 1.0
	modelRatioString := ""
	quota := 0
	token, err := model.GetTokenById(meta.TokenId)
	if err != nil {
		log.Println("获取token出错:", err)
	}
	BillingByRequestEnabled, _ := strconv.ParseBool(common.OptionMap["BillingByRequestEnabled"])
	ModelRatioEnabled, _ := strconv.ParseBool(common.OptionMap["ModelRatioEnabled"])

	if BillingByRequestEnabled && ModelRatioEnabled {
		if token.BillingEnabled {
			modelRatio2, ok := common.GetModelRatio2(imageRequest.Model)
			if !ok { // 如果 ModelRatio2 中没有对应的 name，则继续使用之前的 quota 值
				quota = int(ratio*sizeRatio*imageCostRatio*1000) * imageRequest.N
				modelRatioString = fmt.Sprintf("模型倍率 %.2f", modelRatio)
			} else {
				ratio = modelRatio2 * groupRatio
				quota = int(ratio * common.QuotaPerUnit)
				modelRatioString = fmt.Sprintf("按次计费")
			}
		} else {
			quota = int(ratio*sizeRatio*imageCostRatio*1000) * imageRequest.N
			modelRatioString = fmt.Sprintf("模型倍率 %.2f", modelRatio)
		}
	} else if BillingByRequestEnabled {
		modelRatio2, ok := common.GetModelRatio2(imageRequest.Model)
		if !ok { // 如果 ModelRatio2 中没有对应的 name，则继续使用之前的 quota 值
			quota = int(ratio*sizeRatio*imageCostRatio*1000) * imageRequest.N
			modelRatioString = fmt.Sprintf("模型倍率 %.2f", modelRatio)
		} else {
			ratio = modelRatio2 * groupRatio
			quota = int(ratio * 1 * 500000)
			modelRatioString = fmt.Sprintf("按次计费")
		}
	} else {
		quota = int(ratio*sizeRatio*imageCostRatio*1000) * imageRequest.N
		modelRatioString = fmt.Sprintf("模型倍率 %.2f", modelRatio)
	}

	if userQuota-quota < 0 {
		return openai.ErrorWrapper(errors.New("user quota is not enough"), "insufficient_user_quota", http.StatusForbidden)
	}

	// do request
	resp, err := adaptor.DoRequest(c, meta, requestBody)
	if err != nil {
		logger.Errorf(ctx, "DoRequest failed: %s", err.Error())
		return openai.ErrorWrapper(err, "do_request_failed", http.StatusInternalServerError)
	}

	defer func(ctx context.Context) {
		useTimeSeconds := time.Now().Unix() - startTime.Unix()
		if resp.StatusCode != http.StatusOK {
			return
		}
		err := model.PostConsumeTokenQuota(meta.TokenId, quota)
		if err != nil {
			common.SysError("error consuming token remain quota: " + err.Error())
		}
		err = model.CacheUpdateUserQuota(c, meta.UserId)
		if err != nil {
			common.SysError("error update user quota cache: " + err.Error())
		}
		if quota != 0 {
			tokenName := c.GetString("token_name")
			multiplier := fmt.Sprintf(" %s，分组倍率 %.2f", modelRatioString, groupRatio)
			logContent := fmt.Sprintf(" ")
			model.RecordConsumeLog(ctx, meta.UserId, meta.ChannelId, channelName, 0, 0, imageRequest.Model, tokenName, quota, logContent, meta.TokenId, multiplier, userQuota, int(useTimeSeconds), false)
			model.UpdateUserUsedQuotaAndRequestCount(meta.UserId, quota)
			channelId := c.GetInt("channel_id")
			model.UpdateChannelUsedQuota(channelId, quota)
		}
	}(c.Request.Context())

	// do response
	_, _, respErr := adaptor.DoResponse(c, resp, meta)
	if respErr != nil {
		logger.Errorf(ctx, "respErr is not nil: %+v", respErr)
		return respErr
	}

	return nil
}
