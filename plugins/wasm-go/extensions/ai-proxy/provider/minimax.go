package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/alibaba/higress/plugins/wasm-go/extensions/ai-proxy/util"
	"github.com/alibaba/higress/plugins/wasm-go/pkg/wrapper"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// minimaxProvider is the provider for minimax service.

const (
	minimaxApiTypeV2  = "v2"  // minimaxApiTypeV2 represents chat completion V2 API.
	minimaxApiTypePro = "pro" // minimaxApiTypePro represents chat completion Pro API.
	minimaxDomain     = "api.minimax.chat"
	// minimaxChatCompletionV2Path represents the API path for chat completion V2 API which has a response format similar to OpenAI's.
	minimaxChatCompletionV2Path = "/v1/text/chatcompletion_v2"
	// minimaxChatCompletionProPath represents the API path for chat completion Pro API which has a different response format from OpenAI's.
	minimaxChatCompletionProPath = "/v1/text/chatcompletion_pro"

	senderTypeUser string = "USER" // Content sent by the user.
	senderTypeBot  string = "BOT"  // Content generated by the model.

	// Default bot settings.
	defaultBotName           string = "MM智能助理"
	defaultBotSettingContent string = "MM智能助理是一款由MiniMax自研的，没有调用其他产品的接口的大型语言模型。MiniMax是一家中国科技公司，一直致力于进行大模型相关的研究。"
	defaultSenderName        string = "小明"
)

type minimaxProviderInitializer struct {
}

func (m *minimaxProviderInitializer) ValidateConfig(config ProviderConfig) error {
	// If using the chat completion Pro API, a group ID must be set.
	if minimaxApiTypePro == config.minimaxApiType && config.minimaxGroupId == "" {
		return errors.New(fmt.Sprintf("missing minimaxGroupId in provider config when minimaxApiType is %s", minimaxApiTypePro))
	}
	if config.apiTokens == nil || len(config.apiTokens) == 0 {
		return errors.New("no apiToken found in provider config")
	}
	return nil
}

func (m *minimaxProviderInitializer) CreateProvider(config ProviderConfig) (Provider, error) {
	return &minimaxProvider{
		config:       config,
		contextCache: createContextCache(&config),
	}, nil
}

type minimaxProvider struct {
	config       ProviderConfig
	contextCache *contextCache
}

func (m *minimaxProvider) GetProviderType() string {
	return providerTypeMinimax
}

func (m *minimaxProvider) OnRequestHeaders(ctx wrapper.HttpContext, apiName ApiName, log wrapper.Log) (types.Action, error) {
	if apiName != ApiNameChatCompletion {
		return types.ActionContinue, errUnsupportedApiName
	}
	m.config.handleRequestHeaders(m, ctx, apiName, log)
	// Delay the header processing to allow changing streaming mode in OnRequestBody
	return types.HeaderStopIteration, nil
}

func (m *minimaxProvider) TransformRequestHeaders(ctx wrapper.HttpContext, apiName ApiName, headers http.Header, log wrapper.Log) {
	util.OverwriteRequestHostHeader(headers, minimaxDomain)
	util.OverwriteRequestAuthorizationHeader(headers, "Bearer "+m.config.GetApiTokenInUse(ctx))
	headers.Del("Content-Length")
}

func (m *minimaxProvider) OnRequestBody(ctx wrapper.HttpContext, apiName ApiName, body []byte, log wrapper.Log) (types.Action, error) {
	if apiName != ApiNameChatCompletion {
		return types.ActionContinue, errUnsupportedApiName
	}
	if minimaxApiTypePro == m.config.minimaxApiType {
		// Use chat completion Pro API.
		return m.handleRequestBodyByChatCompletionPro(body, log)
	} else {
		// Use chat completion V2 API.
		return m.config.handleRequestBody(m, m.contextCache, ctx, apiName, body, log)
	}
}

func (m *minimaxProvider) TransformRequestBodyHeaders(ctx wrapper.HttpContext, apiName ApiName, body []byte, headers http.Header, log wrapper.Log) ([]byte, error) {
	return m.handleRequestBodyByChatCompletionV2(body, headers, log)
}

// handleRequestBodyByChatCompletionPro processes the request body using the chat completion Pro API.
func (m *minimaxProvider) handleRequestBodyByChatCompletionPro(body []byte, log wrapper.Log) (types.Action, error) {
	request := &chatCompletionRequest{}
	if err := decodeChatCompletionRequest(body, request); err != nil {
		return types.ActionContinue, err
	}

	// Map the model and rewrite the request path.
	request.Model = getMappedModel(request.Model, m.config.modelMapping, log)
	_ = util.OverwriteRequestPath(fmt.Sprintf("%s?GroupId=%s", minimaxChatCompletionProPath, m.config.minimaxGroupId))

	if m.config.context == nil {
		minimaxRequest := m.buildMinimaxChatCompletionV2Request(request, "")
		return types.ActionContinue, replaceJsonRequestBody(minimaxRequest, log)
	}

	err := m.contextCache.GetContent(func(content string, err error) {
		defer func() {
			_ = proxywasm.ResumeHttpRequest()
		}()
		if err != nil {
			log.Errorf("failed to load context file: %v", err)
			util.ErrorHandler("ai-proxy.minimax.load_ctx_failed", fmt.Errorf("failed to load context file: %v", err))
		}
		// Since minimaxChatCompletionV2 (format consistent with OpenAI) and minimaxChatCompletionPro (different format from OpenAI) have different logic for insertHttpContextMessage, we cannot unify them within one provider.
		// For minimaxChatCompletionPro, we need to manually handle context messages.
		// minimaxChatCompletionV2 uses the default defaultInsertHttpContextMessage method to insert context messages.
		minimaxRequest := m.buildMinimaxChatCompletionV2Request(request, content)
		if err := replaceJsonRequestBody(minimaxRequest, log); err != nil {
			util.ErrorHandler("ai-proxy.minimax.insert_ctx_failed", fmt.Errorf("failed to replace Request body: %v", err))
		}
	}, log)
	if err == nil {
		return types.ActionPause, nil
	}
	return types.ActionContinue, err
}

// handleRequestBodyByChatCompletionV2 processes the request body using the chat completion V2 API.
func (m *minimaxProvider) handleRequestBodyByChatCompletionV2(body []byte, headers http.Header, log wrapper.Log) ([]byte, error) {
	util.OverwriteRequestPathHeader(headers, minimaxChatCompletionV2Path)

	rawModel := gjson.GetBytes(body, "model").String()
	mappedModel := getMappedModel(rawModel, m.config.modelMapping, log)
	return sjson.SetBytes(body, "model", mappedModel)
}

func (m *minimaxProvider) OnResponseHeaders(ctx wrapper.HttpContext, apiName ApiName, log wrapper.Log) (types.Action, error) {
	// Skip OnStreamingResponseBody() and OnResponseBody() when using original protocol.
	if m.config.protocol == protocolOriginal {
		ctx.DontReadResponseBody()
		return types.ActionContinue, nil
	}
	// Skip OnStreamingResponseBody() and OnResponseBody() when the model corresponds to the chat completion V2 interface.
	if minimaxApiTypePro != m.config.minimaxApiType {
		ctx.DontReadResponseBody()
		return types.ActionContinue, nil
	}
	_ = proxywasm.RemoveHttpResponseHeader("Content-Length")
	return types.ActionContinue, nil
}

// OnStreamingResponseBody handles streaming response chunks from the Minimax service only for requests using the OpenAI protocol and corresponding to the chat completion Pro API.
func (m *minimaxProvider) OnStreamingResponseBody(ctx wrapper.HttpContext, name ApiName, chunk []byte, isLastChunk bool, log wrapper.Log) ([]byte, error) {
	if isLastChunk || len(chunk) == 0 {
		return nil, nil
	}
	// Sample event response:
	// data: {"created":1689747645,"model":"abab6.5s-chat","reply":"","choices":[{"messages":[{"sender_type":"BOT","sender_name":"MM智能助理","text":"am from China."}]}],"output_sensitive":false}

	// Sample end event response:
	// data: {"created":1689747645,"model":"abab6.5s-chat","reply":"I am from China.","choices":[{"finish_reason":"stop","messages":[{"sender_type":"BOT","sender_name":"MM智能助理","text":"I am from China."}]}],"usage":{"total_tokens":187},"input_sensitive":false,"output_sensitive":false,"id":"0106b3bc9fd844a9f3de1aa06004e2ab","base_resp":{"status_code":0,"status_msg":""}}
	responseBuilder := &strings.Builder{}
	lines := strings.Split(string(chunk), "\n")
	for _, data := range lines {
		if len(data) < 6 {
			// Ignore blank line or improperly formatted lines.
			continue
		}
		data = data[6:]
		var minimaxResp minimaxChatCompletionV2Resp
		if err := json.Unmarshal([]byte(data), &minimaxResp); err != nil {
			log.Errorf("unable to unmarshal minimax response: %v", err)
			continue
		}
		response := m.responseV2ToOpenAI(&minimaxResp)
		responseBody, err := json.Marshal(response)
		if err != nil {
			log.Errorf("unable to marshal response: %v", err)
			return nil, err
		}
		m.appendResponse(responseBuilder, string(responseBody))
	}
	modifiedResponseChunk := responseBuilder.String()
	log.Debugf("=== modified response chunk: %s", modifiedResponseChunk)
	return []byte(modifiedResponseChunk), nil
}

// OnResponseBody handles the final response body from the Minimax service only for requests using the OpenAI protocol and corresponding to the chat completion Pro API.
func (m *minimaxProvider) OnResponseBody(ctx wrapper.HttpContext, apiName ApiName, body []byte, log wrapper.Log) (types.Action, error) {
	minimaxResp := &minimaxChatCompletionV2Resp{}
	if err := json.Unmarshal(body, minimaxResp); err != nil {
		return types.ActionContinue, fmt.Errorf("unable to unmarshal minimax response: %v", err)
	}
	if minimaxResp.BaseResp.StatusCode != 0 {
		return types.ActionContinue, fmt.Errorf("minimax response error, error_code: %d, error_message: %s", minimaxResp.BaseResp.StatusCode, minimaxResp.BaseResp.StatusMsg)
	}
	response := m.responseV2ToOpenAI(minimaxResp)
	return types.ActionContinue, replaceJsonResponseBody(response, log)
}

// minimaxChatCompletionV2Request represents the structure of a chat completion V2 request.
type minimaxChatCompletionV2Request struct {
	Model             string                  `json:"model"`
	Stream            bool                    `json:"stream,omitempty"`
	TokensToGenerate  int64                   `json:"tokens_to_generate,omitempty"`
	Temperature       float64                 `json:"temperature,omitempty"`
	TopP              float64                 `json:"top_p,omitempty"`
	MaskSensitiveInfo bool                    `json:"mask_sensitive_info"` // Whether to mask sensitive information, defaults to true.
	Messages          []minimaxMessage        `json:"messages"`
	BotSettings       []minimaxBotSetting     `json:"bot_setting"`
	ReplyConstraints  minimaxReplyConstraints `json:"reply_constraints"`
}

// minimaxMessage represents a message in the conversation.
type minimaxMessage struct {
	SenderType string `json:"sender_type"`
	SenderName string `json:"sender_name"`
	Text       string `json:"text"`
}

// minimaxBotSetting represents the bot's settings.
type minimaxBotSetting struct {
	BotName string `json:"bot_name"`
	Content string `json:"content"`
}

// minimaxReplyConstraints represents requirements for model replies.
type minimaxReplyConstraints struct {
	SenderType string `json:"sender_type"`
	SenderName string `json:"sender_name"`
}

// minimaxChatCompletionV2Resp represents the structure of a Minimax Chat Completion V2 response.
type minimaxChatCompletionV2Resp struct {
	Created             int64           `json:"created"`
	Model               string          `json:"model"`
	Reply               string          `json:"reply"`
	InputSensitive      bool            `json:"input_sensitive,omitempty"`
	InputSensitiveType  int64           `json:"input_sensitive_type,omitempty"`
	OutputSensitive     bool            `json:"output_sensitive,omitempty"`
	OutputSensitiveType int64           `json:"output_sensitive_type,omitempty"`
	Choices             []minimaxChoice `json:"choices,omitempty"`
	Usage               minimaxUsage    `json:"usage,omitempty"`
	Id                  string          `json:"id"`
	BaseResp            minimaxBaseResp `json:"base_resp"`
}

// minimaxBaseResp contains error status code and details.
type minimaxBaseResp struct {
	StatusCode int64  `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

// minimaxChoice represents a result option.
type minimaxChoice struct {
	Messages     []minimaxMessage `json:"messages"`
	Index        int64            `json:"index"`
	FinishReason string           `json:"finish_reason"`
}

// minimaxUsage represents token usage statistics.
type minimaxUsage struct {
	TotalTokens int64 `json:"total_tokens"`
}

func (m *minimaxProvider) parseModel(body []byte) (string, error) {
	var tempMap map[string]interface{}
	if err := json.Unmarshal(body, &tempMap); err != nil {
		return "", err
	}
	model, ok := tempMap["model"].(string)
	if !ok {
		return "", errors.New("missing model in chat completion request")
	}
	return model, nil
}

func (m *minimaxProvider) setBotSettings(request *minimaxChatCompletionV2Request, botSettingContent string) {
	if len(request.BotSettings) == 0 {
		request.BotSettings = []minimaxBotSetting{
			{
				BotName: defaultBotName,
				Content: func() string {
					if botSettingContent != "" {
						return botSettingContent
					}
					return defaultBotSettingContent
				}(),
			},
		}
	} else if botSettingContent != "" {
		newSetting := minimaxBotSetting{
			BotName: request.BotSettings[0].BotName,
			Content: botSettingContent,
		}
		request.BotSettings = append([]minimaxBotSetting{newSetting}, request.BotSettings...)
	}
}

func (m *minimaxProvider) buildMinimaxChatCompletionV2Request(request *chatCompletionRequest, botSettingContent string) *minimaxChatCompletionV2Request {
	var messages []minimaxMessage
	var botSetting []minimaxBotSetting
	var botName string

	determineName := func(name string, defaultName string) string {
		if name != "" {
			return name
		}
		return defaultName
	}

	for _, message := range request.Messages {
		switch message.Role {
		case roleSystem:
			botName = determineName(message.Name, defaultBotName)
			botSetting = append(botSetting, minimaxBotSetting{
				BotName: botName,
				Content: message.StringContent(),
			})
		case roleAssistant:
			messages = append(messages, minimaxMessage{
				SenderType: senderTypeBot,
				SenderName: determineName(message.Name, defaultBotName),
				Text:       message.StringContent(),
			})
		case roleUser:
			messages = append(messages, minimaxMessage{
				SenderType: senderTypeUser,
				SenderName: determineName(message.Name, defaultSenderName),
				Text:       message.StringContent(),
			})
		}
	}

	replyConstraints := minimaxReplyConstraints{
		SenderType: senderTypeBot,
		SenderName: determineName(botName, defaultBotName),
	}
	result := &minimaxChatCompletionV2Request{
		Model:             request.Model,
		Stream:            request.Stream,
		TokensToGenerate:  int64(request.MaxTokens),
		Temperature:       request.Temperature,
		TopP:              request.TopP,
		MaskSensitiveInfo: true,
		Messages:          messages,
		BotSettings:       botSetting,
		ReplyConstraints:  replyConstraints,
	}

	m.setBotSettings(result, botSettingContent)
	return result
}

func (m *minimaxProvider) responseV2ToOpenAI(response *minimaxChatCompletionV2Resp) *chatCompletionResponse {
	var choices []chatCompletionChoice
	messageIndex := 0
	for _, choice := range response.Choices {
		for _, message := range choice.Messages {
			message := &chatMessage{
				Name:    message.SenderName,
				Role:    roleAssistant,
				Content: message.Text,
			}
			choices = append(choices, chatCompletionChoice{
				FinishReason: choice.FinishReason,
				Index:        messageIndex,
				Message:      message,
			})
			messageIndex++
		}
	}
	return &chatCompletionResponse{
		Id:      response.Id,
		Object:  objectChatCompletion,
		Created: response.Created,
		Model:   response.Model,
		Choices: choices,
		Usage: usage{
			TotalTokens: int(response.Usage.TotalTokens),
		},
	}
}

func (m *minimaxProvider) appendResponse(responseBuilder *strings.Builder, responseBody string) {
	responseBuilder.WriteString(fmt.Sprintf("%s %s\n\n", streamDataItemKey, responseBody))
}

func (m *minimaxProvider) GetApiName(path string) ApiName {
	if strings.Contains(path, minimaxChatCompletionV2Path) || strings.Contains(path, minimaxChatCompletionProPath) {
		return ApiNameChatCompletion
	}
	return ""
}
