package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

func buildOpenAIChatCompletionsURL(base string) string {
	normalized := strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(normalized, "/chat/completions") {
		return normalized
	}
	if strings.HasSuffix(normalized, "/v1") {
		return normalized + "/chat/completions"
	}
	return normalized + "/v1/chat/completions"
}

const cryptoEnhancedPromptCacheKeyPrefix = "crypto_cc_"

type OpenAICryptoChatPreparation struct {
	EnhancedBody   []byte
	PrefetchResult *OpenAIForwardResult
}

type openAICryptoChatFetchResult struct {
	CryptoData     json.RawMessage
	PrefetchResult *OpenAIForwardResult
}

func deriveCryptoEnhancedPromptCacheKey(originalPromptCacheKey string, enhancedBody []byte) string {
	seed := strings.TrimSpace(originalPromptCacheKey)
	if seed == "" {
		return ""
	}
	if len(bytes.TrimSpace(enhancedBody)) == 0 {
		return seed
	}

	normalized := normalizeCompatSeedJSON(json.RawMessage(enhancedBody))
	if normalized == "" {
		normalized = string(bytes.TrimSpace(enhancedBody))
	}
	sum := sha256.Sum256([]byte(seed + "|" + normalized))
	return cryptoEnhancedPromptCacheKeyPrefix + hex.EncodeToString(sum[:])
}

func (s *OpenAIGatewayService) DeriveCryptoEnhancedPromptCacheKey(originalPromptCacheKey string, enhancedBody []byte) string {
	return deriveCryptoEnhancedPromptCacheKey(originalPromptCacheKey, enhancedBody)
}

func resolveOpenAICryptoPrefetchModel(account *Account, requestedModel string) string {
	reqModel := strings.TrimSpace(requestedModel)
	if account == nil {
		return reqModel
	}

	if configuredModel := strings.TrimSpace(account.GetCredential("model")); configuredModel != "" {
		return configuredModel
	}

	if mappedModel, matched := account.ResolveMappedModel(reqModel); matched {
		return strings.TrimSpace(mappedModel)
	}

	return reqModel
}

func formatCryptoDataSystemMessage(cryptoData json.RawMessage) string {
	trimmed := bytes.TrimSpace(cryptoData)
	if len(trimmed) == 0 {
		return "Use the following crypto_data as supplemental context for the user's request.\n<crypto_data>\n{}\n</crypto_data>"
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err != nil {
		compact.Write(trimmed)
	}

	return "Use the following crypto_data as supplemental context for the user's request.\n<crypto_data>\n" +
		compact.String() +
		"\n</crypto_data>"
}

func injectCryptoDataSystemMessage(body []byte, cryptoData json.RawMessage) ([]byte, error) {
	var chatReq apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &chatReq); err != nil {
		return nil, fmt.Errorf("parse chat completions request: %w", err)
	}

	systemContent, err := json.Marshal(formatCryptoDataSystemMessage(cryptoData))
	if err != nil {
		return nil, fmt.Errorf("marshal crypto data system message: %w", err)
	}

	insertAt := 0
	for insertAt < len(chatReq.Messages) {
		if !strings.EqualFold(strings.TrimSpace(chatReq.Messages[insertAt].Role), "system") {
			break
		}
		insertAt++
	}

	messages := make([]apicompat.ChatMessage, 0, len(chatReq.Messages)+1)
	messages = append(messages, chatReq.Messages[:insertAt]...)
	messages = append(messages, apicompat.ChatMessage{
		Role:    "system",
		Content: systemContent,
	})
	messages = append(messages, chatReq.Messages[insertAt:]...)
	chatReq.Messages = messages

	enhancedBody, err := json.Marshal(&chatReq)
	if err != nil {
		return nil, fmt.Errorf("marshal enhanced chat completions request: %w", err)
	}
	return enhancedBody, nil
}

func (s *OpenAIGatewayService) PrepareCryptoEnhancedChatRequest(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
) (*OpenAICryptoChatPreparation, error) {
	fetchResult, err := s.fetchCryptoDataForChatCompletions(ctx, c, account, body)
	if err != nil {
		return nil, err
	}
	enhancedBody, err := injectCryptoDataSystemMessage(body, fetchResult.CryptoData)
	if err != nil {
		return nil, err
	}

	return &OpenAICryptoChatPreparation{
		EnhancedBody:   enhancedBody,
		PrefetchResult: fetchResult.PrefetchResult,
	}, nil
}

func (s *OpenAIGatewayService) PrepareCryptoEnhancedChatRequestBody(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
) ([]byte, error) {
	prepared, err := s.PrepareCryptoEnhancedChatRequest(ctx, c, account, body)
	if err != nil {
		return nil, err
	}
	if prepared == nil {
		return nil, fmt.Errorf("crypto chat preparation is nil")
	}
	return prepared.EnhancedBody, nil
}

func (s *OpenAIGatewayService) fetchCryptoDataForChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
) (*openAICryptoChatFetchResult, error) {
	if account == nil {
		return nil, fmt.Errorf("account is required")
	}
	startTime := time.Now()

	reqModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	prefetchModel := resolveOpenAICryptoPrefetchModel(account, reqModel)

	forwardBody := body
	var err error
	if prefetchModel != "" && prefetchModel != reqModel {
		forwardBody, err = sjson.SetBytes(forwardBody, "model", prefetchModel)
		if err != nil {
			return nil, fmt.Errorf("set mapped model: %w", err)
		}
	}
	forwardBody, err = sjson.SetBytes(forwardBody, "stream", false)
	if err != nil {
		return nil, fmt.Errorf("disable upstream stream: %w", err)
	}

	token := ""
	requiresAuth := true
	if account.IsOpenAICryptoRouter() && account.IsOpenAIApiKey() && strings.TrimSpace(account.GetOpenAIApiKey()) == "" {
		requiresAuth = false
	} else {
		token, _, err = s.GetAccessToken(ctx, account)
		if err != nil {
			return nil, err
		}
	}

	upstreamReq, err := s.buildChatCompletionsPassthroughRequest(ctx, c, account, forwardBody, token, requiresAuth)
	if err != nil {
		return nil, err
	}

	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		safeErr := sanitizeUpstreamErrorMessage(err.Error())
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: 0,
			Passthrough:        true,
			Kind:               "request_error",
			Message:            safeErr,
		})
		return nil, fmt.Errorf("upstream request failed: %s", safeErr)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
		upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
		if s.shouldFailoverOpenAIUpstreamResponse(resp.StatusCode, upstreamMsg, respBody) {
			upstreamDetail := ""
			if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
				maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
				if maxBytes <= 0 {
					maxBytes = 2048
				}
				upstreamDetail = truncateString(string(respBody), maxBytes)
			}
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  resp.Header.Get("x-request-id"),
				Passthrough:        true,
				Kind:               "failover",
				Message:            upstreamMsg,
				Detail:             upstreamDetail,
			})
			if s.rateLimitService != nil {
				s.rateLimitService.HandleUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
			}
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && (isPoolModeRetryableStatus(resp.StatusCode) || isOpenAITransientProcessingError(resp.StatusCode, upstreamMsg, respBody)),
			}
		}
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d message=%s", resp.StatusCode, upstreamMsg)
	}

	maxBytes := resolveUpstreamResponseReadLimit(s.cfg)
	respBody, err := readUpstreamResponseBodyLimited(resp.Body, maxBytes)
	if err != nil {
		return nil, err
	}
	respBody, err = normalizeCryptoChatPrefetchResponseBody(resp.Header, respBody)
	if err != nil {
		return nil, err
	}

	var payload struct {
		Model  string               `json:"model"`
		Usage  *apicompat.ChatUsage `json:"usage"`
		Crypto struct {
			CryptoData json.RawMessage `json:"crypto_data"`
		} `json:"crypto"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil, fmt.Errorf("parse crypto provider response: %w", err)
	}

	cryptoData := bytes.TrimSpace(payload.Crypto.CryptoData)
	if len(cryptoData) == 0 || bytes.Equal(cryptoData, []byte("null")) {
		return nil, fmt.Errorf("crypto provider response missing crypto.crypto_data")
	}

	out := make(json.RawMessage, len(cryptoData))
	copy(out, cryptoData)
	billingModel := strings.TrimSpace(payload.Model)
	if billingModel == "" {
		billingModel = prefetchModel
	}

	return &openAICryptoChatFetchResult{
		CryptoData: out,
		PrefetchResult: &OpenAIForwardResult{
			RequestID:     resp.Header.Get("x-request-id"),
			Usage:         openAIUsageFromChatUsage(payload.Usage),
			Model:         billingModel,
			BillingModel:  billingModel,
			UpstreamModel: billingModel,
			Stream:        false,
			Duration:      time.Since(startTime),
		},
	}, nil
}

func normalizeCryptoChatPrefetchResponseBody(headers http.Header, body []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("crypto provider response is empty")
	}
	if !isEventStreamResponse(headers) && !bytes.HasPrefix(trimmed, []byte("data:")) && !bytes.Contains(trimmed, []byte("\ndata:")) {
		return trimmed, nil
	}

	scanner := bufio.NewScanner(bytes.NewReader(trimmed))
	scanner.Buffer(make([]byte, 0, 64*1024), defaultMaxLineSize)

	var lastJSON []byte
	for scanner.Scan() {
		data, ok := extractOpenAISSEDataLine(scanner.Text())
		if !ok || data == "" || data == "[DONE]" {
			continue
		}
		if !gjson.Valid(data) {
			continue
		}
		lastJSON = append(lastJSON[:0], data...)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read crypto provider stream: %w", err)
	}
	if len(lastJSON) == 0 {
		return nil, fmt.Errorf("crypto provider stream missing terminal json payload")
	}
	return lastJSON, nil
}

// ForwardAsChatCompletions accepts a Chat Completions request body, converts it
// to OpenAI Responses API format, forwards to the OpenAI upstream, and converts
// the response back to Chat Completions format. All account types (OAuth and API
// Key) go through the Responses API conversion path since the upstream only
// exposes the /v1/responses endpoint.
func (s *OpenAIGatewayService) ForwardAsChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	promptCacheKey string,
	defaultMappedModel string,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()

	// 1. Parse Chat Completions request
	var chatReq apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &chatReq); err != nil {
		return nil, fmt.Errorf("parse chat completions request: %w", err)
	}
	originalModel := chatReq.Model
	clientStream := chatReq.Stream
	includeUsage := chatReq.StreamOptions != nil && chatReq.StreamOptions.IncludeUsage

	// 2. Resolve model mapping early so compat prompt_cache_key injection can
	// derive a stable seed from the final upstream model family.
	mappedModel := resolveOpenAIForwardModel(account, originalModel, defaultMappedModel)

	promptCacheKey = strings.TrimSpace(promptCacheKey)
	compatPromptCacheInjected := false
	if promptCacheKey == "" && account.Type == AccountTypeOAuth && shouldAutoInjectPromptCacheKeyForCompat(mappedModel) {
		promptCacheKey = deriveCompatPromptCacheKey(&chatReq, mappedModel)
		compatPromptCacheInjected = promptCacheKey != ""
	}

	// 3. Convert to Responses and forward
	// ChatCompletionsToResponses always sets Stream=true (upstream always streams).
	responsesReq, err := apicompat.ChatCompletionsToResponses(&chatReq)
	if err != nil {
		return nil, fmt.Errorf("convert chat completions to responses: %w", err)
	}
	responsesReq.Model = mappedModel

	logFields := []zap.Field{
		zap.Int64("account_id", account.ID),
		zap.String("original_model", originalModel),
		zap.String("mapped_model", mappedModel),
		zap.Bool("stream", clientStream),
	}
	if compatPromptCacheInjected {
		logFields = append(logFields,
			zap.Bool("compat_prompt_cache_key_injected", true),
			zap.String("compat_prompt_cache_key_sha256", hashSensitiveValueForLog(promptCacheKey)),
		)
	}
	logger.L().Debug("openai chat_completions: model mapping applied", logFields...)

	// 4. Marshal Responses request body, then apply OAuth codex transform
	responsesBody, err := json.Marshal(responsesReq)
	if err != nil {
		return nil, fmt.Errorf("marshal responses request: %w", err)
	}

	if account.Type == AccountTypeOAuth {
		var reqBody map[string]any
		if err := json.Unmarshal(responsesBody, &reqBody); err != nil {
			return nil, fmt.Errorf("unmarshal for codex transform: %w", err)
		}
		codexResult := applyCodexOAuthTransform(reqBody, false, false)
		if codexResult.PromptCacheKey != "" {
			promptCacheKey = codexResult.PromptCacheKey
		} else if promptCacheKey != "" {
			reqBody["prompt_cache_key"] = promptCacheKey
		}
		responsesBody, err = json.Marshal(reqBody)
		if err != nil {
			return nil, fmt.Errorf("remarshal after codex transform: %w", err)
		}
	}

	// 5. Get access token
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("get access token: %w", err)
	}

	// 6. Build upstream request
	upstreamReq, err := s.buildUpstreamRequest(ctx, c, account, responsesBody, token, true, promptCacheKey, false)
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}

	if promptCacheKey != "" {
		upstreamReq.Header.Set("session_id", generateSessionUUID(promptCacheKey))
	}

	// 7. Send request
	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		safeErr := sanitizeUpstreamErrorMessage(err.Error())
		setOpsUpstreamError(c, 0, safeErr, "")
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: 0,
			Kind:               "request_error",
			Message:            safeErr,
		})
		writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
		return nil, fmt.Errorf("upstream request failed: %s", safeErr)
	}
	defer func() { _ = resp.Body.Close() }()

	// 8. Handle error response with failover
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
		upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
		if s.shouldFailoverOpenAIUpstreamResponse(resp.StatusCode, upstreamMsg, respBody) {
			upstreamDetail := ""
			if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
				maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
				if maxBytes <= 0 {
					maxBytes = 2048
				}
				upstreamDetail = truncateString(string(respBody), maxBytes)
			}
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  resp.Header.Get("x-request-id"),
				Kind:               "failover",
				Message:            upstreamMsg,
				Detail:             upstreamDetail,
			})
			if s.rateLimitService != nil {
				s.rateLimitService.HandleUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
			}
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && (isPoolModeRetryableStatus(resp.StatusCode) || isOpenAITransientProcessingError(resp.StatusCode, upstreamMsg, respBody)),
			}
		}
		return s.handleChatCompletionsErrorResponse(resp, c, account)
	}

	// 9. Handle normal response
	var result *OpenAIForwardResult
	var handleErr error
	if clientStream {
		result, handleErr = s.handleChatStreamingResponse(resp, c, originalModel, mappedModel, includeUsage, startTime)
	} else {
		result, handleErr = s.handleChatBufferedStreamingResponse(resp, c, originalModel, mappedModel, startTime)
	}

	// Propagate ServiceTier and ReasoningEffort to result for billing
	if handleErr == nil && result != nil {
		if responsesReq.ServiceTier != "" {
			st := responsesReq.ServiceTier
			result.ServiceTier = &st
		}
		if responsesReq.Reasoning != nil && responsesReq.Reasoning.Effort != "" {
			re := responsesReq.Reasoning.Effort
			result.ReasoningEffort = &re
		}
	}

	// Extract and save Codex usage snapshot from response headers (for OAuth accounts)
	if handleErr == nil && account.Type == AccountTypeOAuth {
		if snapshot := ParseCodexRateLimitHeaders(resp.Header); snapshot != nil {
			s.updateCodexUsageSnapshot(ctx, account.ID, snapshot)
		}
	}

	return result, handleErr
}

// handleChatCompletionsErrorResponse reads an upstream error and returns it in
// OpenAI Chat Completions error format.
func (s *OpenAIGatewayService) handleChatCompletionsErrorResponse(
	resp *http.Response,
	c *gin.Context,
	account *Account,
) (*OpenAIForwardResult, error) {
	return s.handleCompatErrorResponse(resp, c, account, writeChatCompletionsError)
}

// handleChatBufferedStreamingResponse reads all Responses SSE events from the
// upstream, finds the terminal event, converts to a Chat Completions JSON
// response, and writes it to the client.
func (s *OpenAIGatewayService) handleChatBufferedStreamingResponse(
	resp *http.Response,
	c *gin.Context,
	originalModel string,
	mappedModel string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

	var finalResponse *apicompat.ResponsesResponse
	var usage OpenAIUsage

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		payload := line[6:]

		var event apicompat.ResponsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			logger.L().Warn("openai chat_completions buffered: failed to parse event",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
			continue
		}

		if (event.Type == "response.completed" || event.Type == "response.incomplete" || event.Type == "response.failed") &&
			event.Response != nil {
			finalResponse = event.Response
			if event.Response.Usage != nil {
				usage = OpenAIUsage{
					InputTokens:  event.Response.Usage.InputTokens,
					OutputTokens: event.Response.Usage.OutputTokens,
				}
				if event.Response.Usage.InputTokensDetails != nil {
					usage.CacheReadInputTokens = event.Response.Usage.InputTokensDetails.CachedTokens
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			logger.L().Warn("openai chat_completions buffered: read error",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
		}
	}

	if finalResponse == nil {
		writeChatCompletionsError(c, http.StatusBadGateway, "api_error", "Upstream stream ended without a terminal response event")
		return nil, fmt.Errorf("upstream stream ended without terminal event")
	}

	chatResp := apicompat.ResponsesToChatCompletions(finalResponse, originalModel)

	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	c.JSON(http.StatusOK, chatResp)

	return &OpenAIForwardResult{
		RequestID:     requestID,
		Usage:         usage,
		Model:         originalModel,
		BillingModel:  mappedModel,
		UpstreamModel: mappedModel,
		Stream:        false,
		Duration:      time.Since(startTime),
	}, nil
}

// handleChatStreamingResponse reads Responses SSE events from upstream,
// converts each to Chat Completions SSE chunks, and writes them to the client.
func (s *OpenAIGatewayService) handleChatStreamingResponse(
	resp *http.Response,
	c *gin.Context,
	originalModel string,
	mappedModel string,
	includeUsage bool,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")

	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)

	state := apicompat.NewResponsesEventToChatState()
	state.Model = originalModel
	state.IncludeUsage = includeUsage

	var usage OpenAIUsage
	var firstTokenMs *int
	firstChunk := true

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

	resultWithUsage := func() *OpenAIForwardResult {
		return &OpenAIForwardResult{
			RequestID:     requestID,
			Usage:         usage,
			Model:         originalModel,
			BillingModel:  mappedModel,
			UpstreamModel: mappedModel,
			Stream:        true,
			Duration:      time.Since(startTime),
			FirstTokenMs:  firstTokenMs,
		}
	}

	processDataLine := func(payload string) bool {
		if firstChunk {
			firstChunk = false
			ms := int(time.Since(startTime).Milliseconds())
			firstTokenMs = &ms
		}

		var event apicompat.ResponsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			logger.L().Warn("openai chat_completions stream: failed to parse event",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
			return false
		}

		// Extract usage from completion events
		if (event.Type == "response.completed" || event.Type == "response.incomplete" || event.Type == "response.failed") &&
			event.Response != nil && event.Response.Usage != nil {
			usage = OpenAIUsage{
				InputTokens:  event.Response.Usage.InputTokens,
				OutputTokens: event.Response.Usage.OutputTokens,
			}
			if event.Response.Usage.InputTokensDetails != nil {
				usage.CacheReadInputTokens = event.Response.Usage.InputTokensDetails.CachedTokens
			}
		}

		chunks := apicompat.ResponsesEventToChatChunks(&event, state)
		for _, chunk := range chunks {
			sse, err := apicompat.ChatChunkToSSE(chunk)
			if err != nil {
				logger.L().Warn("openai chat_completions stream: failed to marshal chunk",
					zap.Error(err),
					zap.String("request_id", requestID),
				)
				continue
			}
			if _, err := fmt.Fprint(c.Writer, sse); err != nil {
				logger.L().Info("openai chat_completions stream: client disconnected",
					zap.String("request_id", requestID),
				)
				return true
			}
		}
		if len(chunks) > 0 {
			c.Writer.Flush()
		}
		return false
	}

	finalizeStream := func() (*OpenAIForwardResult, error) {
		if finalChunks := apicompat.FinalizeResponsesChatStream(state); len(finalChunks) > 0 {
			for _, chunk := range finalChunks {
				sse, err := apicompat.ChatChunkToSSE(chunk)
				if err != nil {
					continue
				}
				fmt.Fprint(c.Writer, sse) //nolint:errcheck
			}
		}
		// Send [DONE] sentinel
		fmt.Fprint(c.Writer, "data: [DONE]\n\n") //nolint:errcheck
		c.Writer.Flush()
		return resultWithUsage(), nil
	}

	handleScanErr := func(err error) {
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			logger.L().Warn("openai chat_completions stream: read error",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
		}
	}

	// Determine keepalive interval
	keepaliveInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamKeepaliveInterval > 0 {
		keepaliveInterval = time.Duration(s.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	}

	// No keepalive: fast synchronous path
	if keepaliveInterval <= 0 {
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
				continue
			}
			if processDataLine(line[6:]) {
				return resultWithUsage(), nil
			}
		}
		handleScanErr(scanner.Err())
		return finalizeStream()
	}

	// With keepalive: goroutine + channel + select
	type scanEvent struct {
		line string
		err  error
	}
	events := make(chan scanEvent, 16)
	done := make(chan struct{})
	sendEvent := func(ev scanEvent) bool {
		select {
		case events <- ev:
			return true
		case <-done:
			return false
		}
	}
	go func() {
		defer close(events)
		for scanner.Scan() {
			if !sendEvent(scanEvent{line: scanner.Text()}) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			_ = sendEvent(scanEvent{err: err})
		}
	}()
	defer close(done)

	keepaliveTicker := time.NewTicker(keepaliveInterval)
	defer keepaliveTicker.Stop()
	lastDataAt := time.Now()

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return finalizeStream()
			}
			if ev.err != nil {
				handleScanErr(ev.err)
				return finalizeStream()
			}
			lastDataAt = time.Now()
			line := ev.line
			if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
				continue
			}
			if processDataLine(line[6:]) {
				return resultWithUsage(), nil
			}

		case <-keepaliveTicker.C:
			if time.Since(lastDataAt) < keepaliveInterval {
				continue
			}
			// Send SSE comment as keepalive
			if _, err := fmt.Fprint(c.Writer, ":\n\n"); err != nil {
				logger.L().Info("openai chat_completions stream: client disconnected during keepalive",
					zap.String("request_id", requestID),
				)
				return resultWithUsage(), nil
			}
			c.Writer.Flush()
		}
	}
}

func (s *OpenAIGatewayService) ForwardChatCompletionsPassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	defaultMappedModel string,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()
	if account == nil {
		return nil, fmt.Errorf("account is required")
	}

	reqModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	reqStream := gjson.GetBytes(body, "stream").Bool()
	includeUsage := gjson.GetBytes(body, "stream_options.include_usage").Bool()
	mappedModel := resolveOpenAIForwardModel(account, reqModel, defaultMappedModel)

	forwardBody := body
	var err error
	if mappedModel != "" && mappedModel != reqModel {
		forwardBody, err = sjson.SetBytes(forwardBody, "model", mappedModel)
		if err != nil {
			return nil, fmt.Errorf("set mapped model: %w", err)
		}
	}
	if reqStream {
		forwardBody, err = sjson.SetBytes(forwardBody, "stream", false)
		if err != nil {
			return nil, fmt.Errorf("disable upstream stream: %w", err)
		}
	}

	token := ""
	requiresAuth := true
	if account.IsOpenAICryptoRouter() && account.IsOpenAIApiKey() && strings.TrimSpace(account.GetOpenAIApiKey()) == "" {
		requiresAuth = false
	} else {
		token, _, err = s.GetAccessToken(ctx, account)
		if err != nil {
			return nil, err
		}
	}

	upstreamReq, err := s.buildChatCompletionsPassthroughRequest(ctx, c, account, forwardBody, token, requiresAuth)
	if err != nil {
		return nil, err
	}

	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	setOpsUpstreamRequestBody(c, forwardBody)
	if c != nil {
		c.Set("openai_passthrough", true)
	}

	upstreamStart := time.Now()
	resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	if err != nil {
		safeErr := sanitizeUpstreamErrorMessage(err.Error())
		setOpsUpstreamError(c, 0, safeErr, "")
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: 0,
			Passthrough:        true,
			Kind:               "request_error",
			Message:            safeErr,
		})
		writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
		return nil, fmt.Errorf("upstream request failed: %s", safeErr)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
		upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
		if s.shouldFailoverOpenAIUpstreamResponse(resp.StatusCode, upstreamMsg, respBody) {
			upstreamDetail := ""
			if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
				maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
				if maxBytes <= 0 {
					maxBytes = 2048
				}
				upstreamDetail = truncateString(string(respBody), maxBytes)
			}
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  resp.Header.Get("x-request-id"),
				Passthrough:        true,
				Kind:               "failover",
				Message:            upstreamMsg,
				Detail:             upstreamDetail,
			})
			if s.rateLimitService != nil {
				s.rateLimitService.HandleUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
			}
			return nil, &UpstreamFailoverError{StatusCode: resp.StatusCode, ResponseBody: respBody}
		}
		return s.handleChatCompletionsErrorResponse(resp, c, account)
	}

	return s.handleChatCompletionsPassthroughSuccess(ctx, resp, c, reqModel, mappedModel, reqStream, includeUsage, startTime)
}

func (s *OpenAIGatewayService) buildChatCompletionsPassthroughRequest(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
	requiresAuth bool,
) (*http.Request, error) {
	validatedURL, err := s.validateUpstreamBaseURL(account.GetOpenAIBaseURL())
	if err != nil {
		return nil, err
	}
	var targetURL string
	if account.IsOpenAIOAuth() {
		targetURL = buildOpenAIResponsesURL(validatedURL)
	} else {
		targetURL = buildOpenAIChatCompletionsURL(validatedURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	allowTimeoutHeaders := s.isOpenAIPassthroughTimeoutHeadersAllowed()
	if c != nil && c.Request != nil {
		for key, values := range c.Request.Header {
			lower := strings.ToLower(strings.TrimSpace(key))
			if !isOpenAIPassthroughAllowedRequestHeader(lower, allowTimeoutHeaders) {
				continue
			}
			for _, v := range values {
				req.Header.Add(key, v)
			}
		}
	}

	req.Header.Del("authorization")
	req.Header.Del("x-api-key")
	req.Header.Del("x-goog-api-key")
	if requiresAuth {
		req.Header.Set("authorization", "Bearer "+token)
	}
	if customUA := account.GetOpenAIUserAgent(); customUA != "" {
		req.Header.Set("user-agent", customUA)
	}
	if req.Header.Get("content-type") == "" {
		req.Header.Set("content-type", "application/json")
	}

	return req, nil
}

func (s *OpenAIGatewayService) handleChatCompletionsPassthroughSuccess(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	originalModel string,
	mappedModel string,
	clientStream bool,
	includeUsage bool,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	maxBytes := resolveUpstreamResponseReadLimit(s.cfg)
	body, err := readUpstreamResponseBodyLimited(resp.Body, maxBytes)
	if err != nil {
		if errors.Is(err, ErrUpstreamResponseBodyTooLarge) {
			setOpsUpstreamError(c, http.StatusBadGateway, "upstream response too large", "")
			writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", "Upstream response too large")
		}
		return nil, err
	}

	var chatResp apicompat.ChatCompletionsResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return nil, fmt.Errorf("parse chat completions passthrough response: %w", err)
	}
	if mappedModel != "" && originalModel != "" && chatResp.Model == mappedModel {
		chatResp.Model = originalModel
		body, err = json.Marshal(chatResp)
		if err != nil {
			return nil, fmt.Errorf("remarshal chat completions passthrough response: %w", err)
		}
	}

	usage := openAIUsageFromChatUsage(chatResp.Usage)

	if !clientStream {
		writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/json"
		}
		c.Data(resp.StatusCode, contentType, body)
		return &OpenAIForwardResult{
			RequestID:     resp.Header.Get("x-request-id"),
			Usage:         usage,
			Model:         originalModel,
			UpstreamModel: mappedModel,
			Stream:        false,
			OpenAIWSMode:  false,
			Duration:      time.Since(startTime),
		}, nil
	}

	firstTokenMs, err := s.writeChatCompletionsStreamFromResponse(c, resp, &chatResp, originalModel, includeUsage, startTime)
	if err != nil {
		return nil, err
	}
	return &OpenAIForwardResult{
		RequestID:     resp.Header.Get("x-request-id"),
		Usage:         usage,
		Model:         originalModel,
		UpstreamModel: mappedModel,
		Stream:        true,
		OpenAIWSMode:  false,
		Duration:      time.Since(startTime),
		FirstTokenMs:  firstTokenMs,
	}, nil
}

func openAIUsageFromChatUsage(usage *apicompat.ChatUsage) OpenAIUsage {
	if usage == nil {
		return OpenAIUsage{}
	}
	out := OpenAIUsage{
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
	}
	if usage.PromptTokensDetails != nil {
		out.CacheReadInputTokens = usage.PromptTokensDetails.CachedTokens
	}
	return out
}

func (s *OpenAIGatewayService) writeChatCompletionsStreamFromResponse(
	c *gin.Context,
	resp *http.Response,
	chatResp *apicompat.ChatCompletionsResponse,
	originalModel string,
	includeUsage bool,
	startTime time.Time,
) (*int, error) {
	if c == nil || c.Writer == nil {
		return nil, fmt.Errorf("gin context is required")
	}
	if chatResp == nil {
		return nil, fmt.Errorf("chat response is required")
	}

	writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	if v := resp.Header.Get("x-request-id"); v != "" {
		c.Writer.Header().Set("x-request-id", v)
	}

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming not supported")
	}

	if len(chatResp.Choices) == 0 {
		writeChatCompletionsError(c, http.StatusBadGateway, "api_error", "Upstream response missing choices")
		return nil, fmt.Errorf("chat completions passthrough response missing choices")
	}

	choice := chatResp.Choices[0]
	message := choice.Message
	chunks := make([]apicompat.ChatCompletionsChunk, 0, 4)
	chunks = append(chunks, apicompat.ChatCompletionsChunk{
		ID:      chatResp.ID,
		Object:  "chat.completion.chunk",
		Created: chatResp.Created,
		Model:   originalModel,
		Choices: []apicompat.ChatChunkChoice{{
			Index: 0,
			Delta: apicompat.ChatDelta{Role: "assistant"},
		}},
		SystemFingerprint: chatResp.SystemFingerprint,
		ServiceTier:       chatResp.ServiceTier,
	})

	if strings.TrimSpace(message.ReasoningContent) != "" {
		reasoning := message.ReasoningContent
		chunks = append(chunks, apicompat.ChatCompletionsChunk{
			ID:      chatResp.ID,
			Object:  "chat.completion.chunk",
			Created: chatResp.Created,
			Model:   originalModel,
			Choices: []apicompat.ChatChunkChoice{{
				Index: 0,
				Delta: apicompat.ChatDelta{ReasoningContent: &reasoning},
			}},
			SystemFingerprint: chatResp.SystemFingerprint,
			ServiceTier:       chatResp.ServiceTier,
		})
	}

	if content := strings.TrimSpace(extractChatMessageContentText(message.Content)); content != "" {
		text := content
		chunks = append(chunks, apicompat.ChatCompletionsChunk{
			ID:      chatResp.ID,
			Object:  "chat.completion.chunk",
			Created: chatResp.Created,
			Model:   originalModel,
			Choices: []apicompat.ChatChunkChoice{{
				Index: 0,
				Delta: apicompat.ChatDelta{Content: &text},
			}},
			SystemFingerprint: chatResp.SystemFingerprint,
			ServiceTier:       chatResp.ServiceTier,
		})
	}

	if len(message.ToolCalls) > 0 {
		toolCalls := make([]apicompat.ChatToolCall, 0, len(message.ToolCalls))
		for idx, toolCall := range message.ToolCalls {
			copyCall := toolCall
			copyIdx := idx
			copyCall.Index = &copyIdx
			toolCalls = append(toolCalls, copyCall)
		}
		chunks = append(chunks, apicompat.ChatCompletionsChunk{
			ID:      chatResp.ID,
			Object:  "chat.completion.chunk",
			Created: chatResp.Created,
			Model:   originalModel,
			Choices: []apicompat.ChatChunkChoice{{
				Index: 0,
				Delta: apicompat.ChatDelta{ToolCalls: toolCalls},
			}},
			SystemFingerprint: chatResp.SystemFingerprint,
			ServiceTier:       chatResp.ServiceTier,
		})
	}

	finishReason := choice.FinishReason
	chunks = append(chunks, apicompat.ChatCompletionsChunk{
		ID:      chatResp.ID,
		Object:  "chat.completion.chunk",
		Created: chatResp.Created,
		Model:   originalModel,
		Choices: []apicompat.ChatChunkChoice{{
			Index:        0,
			Delta:        apicompat.ChatDelta{},
			FinishReason: &finishReason,
		}},
		SystemFingerprint: chatResp.SystemFingerprint,
		ServiceTier:       chatResp.ServiceTier,
	})

	if includeUsage && chatResp.Usage != nil {
		usageCopy := *chatResp.Usage
		chunks = append(chunks, apicompat.ChatCompletionsChunk{
			ID:                chatResp.ID,
			Object:            "chat.completion.chunk",
			Created:           chatResp.Created,
			Model:             originalModel,
			Choices:           []apicompat.ChatChunkChoice{},
			Usage:             &usageCopy,
			SystemFingerprint: chatResp.SystemFingerprint,
			ServiceTier:       chatResp.ServiceTier,
		})
	}

	var firstTokenMs *int
	for idx, chunk := range chunks {
		if idx == 0 {
			ms := int(time.Since(startTime).Milliseconds())
			firstTokenMs = &ms
		}
		sse, err := apicompat.ChatChunkToSSE(chunk)
		if err != nil {
			return nil, err
		}
		if _, err := fmt.Fprint(c.Writer, sse); err != nil {
			return firstTokenMs, nil
		}
	}
	if _, err := fmt.Fprint(c.Writer, "data: [DONE]\n\n"); err != nil {
		return firstTokenMs, nil
	}
	flusher.Flush()
	return firstTokenMs, nil
}

func extractChatMessageContentText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			return text
		}
		return ""
	}
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &parts); err != nil {
			return ""
		}
		var builder strings.Builder
		for _, part := range parts {
			if strings.TrimSpace(part.Text) == "" {
				continue
			}
			if builder.Len() > 0 {
				builder.WriteByte(' ')
			}
			builder.WriteString(part.Text)
		}
		return builder.String()
	}
	return ""
}

// writeChatCompletionsError writes an error response in OpenAI Chat Completions format.
func writeChatCompletionsError(c *gin.Context, statusCode int, errType, message string) {
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}
