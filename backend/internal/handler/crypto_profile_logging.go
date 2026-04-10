package handler

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const (
	cryptoProfileOriginalMessageContextKey = "crypto_profile_original_message"
	cryptoProfileMatchedContextKey         = "crypto_profile_matched"
	cryptoProfileContextKey                = "crypto_profile"
)

func maybeLogCryptoProfileMatch(
	ctx context.Context,
	reqLog *zap.Logger,
	detector service.CryptoProfileDetector,
	message string,
	entrypoint string,
) {
	_ = detectCryptoProfileMatch(ctx, reqLog, detector, message, entrypoint)
}

func detectCryptoProfileMatch(
	ctx context.Context,
	reqLog *zap.Logger,
	detector service.CryptoProfileDetector,
	message string,
	entrypoint string,
) *service.CryptoProfileMatchResult {
	if reqLog == nil {
		reqLog = logger.L()
	}

	trimmed := strings.TrimSpace(message)
	detectorEnabled := detector != nil && detector.Enabled()
	if detector == nil || !detectorEnabled {
		reason := "detector_disabled"
		if detector == nil {
			reason = "detector_nil"
		}
		reqLog.Info("gateway.crypto_profile_detection_skipped",
			zap.String("entrypoint", entrypoint),
			zap.String("reason", reason),
			zap.Bool("detector_configured", detector != nil),
			zap.Bool("detector_enabled", detectorEnabled),
			zap.Int("message_chars", len(trimmed)),
		)
		return nil
	}

	if trimmed == "" {
		reqLog.Info("gateway.crypto_profile_detection_skipped",
			zap.String("entrypoint", entrypoint),
			zap.String("reason", "empty_message"),
			zap.Bool("detector_configured", true),
			zap.Bool("detector_enabled", true),
			zap.Int("message_chars", 0),
		)
		return nil
	}

	reqLog.Info("gateway.crypto_profile_detection_invoking",
		zap.String("entrypoint", entrypoint),
		zap.Bool("detector_configured", true),
		zap.Bool("detector_enabled", true),
		zap.Int("message_chars", len(trimmed)),
	)

	detectStart := time.Now()
	result, err := detector.Detect(ctx, trimmed)
	detectElapsed := time.Since(detectStart)

	if err != nil {
		reqLog.Warn("gateway.crypto_profile_detection_failed",
			zap.String("entrypoint", entrypoint),
			zap.String("original_message", trimmed),
			zap.Int("message_chars", len(trimmed)),
			zap.String("error_reason", err.Error()),
			zap.Duration("elapsed", detectElapsed),
			zap.Error(err),
		)
		return nil
	}
	if result == nil {
		reqLog.Info("gateway.crypto_profile_detection_empty_result",
			zap.String("entrypoint", entrypoint),
			zap.Int("message_chars", len(trimmed)),
			zap.Duration("elapsed", detectElapsed),
		)
		return nil
	}

	logDetectResult := reqLog.Info
	if detectElapsed >= 50*time.Millisecond {
		logDetectResult = reqLog.Warn
	}

	if !result.Matched {
		logDetectResult("gateway.crypto_profile_not_matched",
			zap.String("entrypoint", entrypoint),
			zap.String("crypto_profile", result.Profile),
			zap.String("detector_model", result.Model),
			zap.Int("message_chars", len(trimmed)),
			zap.Duration("elapsed", detectElapsed),
		)
		return result
	}

	logDetectResult("gateway.crypto_profile_matched",
		zap.String("entrypoint", entrypoint),
		zap.String("crypto_profile", result.Profile),
		zap.String("detector_model", result.Model),
		zap.Int("message_chars", len(trimmed)),
		zap.Duration("elapsed", detectElapsed),
	)
	return result
}

func setCryptoProfileLogContext(c *gin.Context, message string, result *service.CryptoProfileMatchResult) {
	if c == nil {
		return
	}

	trimmed := strings.TrimSpace(message)
	if trimmed != "" {
		c.Set(cryptoProfileOriginalMessageContextKey, trimmed)
	}

	matched := result != nil && result.Matched
	c.Set(cryptoProfileMatchedContextKey, matched)

	if !matched {
		return
	}
	if profile := strings.TrimSpace(result.Profile); profile != "" {
		c.Set(cryptoProfileContextKey, profile)
	}
}

func cryptoUpstreamFailureLogFields(c *gin.Context, errorReason string) ([]zap.Field, bool) {
	if c == nil {
		return nil, false
	}

	rawMatched, ok := c.Get(cryptoProfileMatchedContextKey)
	if !ok {
		return nil, false
	}
	matched, _ := rawMatched.(bool)
	if !matched {
		return nil, false
	}

	fields := make([]zap.Field, 0, 3)
	if rawMessage, ok := c.Get(cryptoProfileOriginalMessageContextKey); ok {
		if originalMessage, ok := rawMessage.(string); ok && strings.TrimSpace(originalMessage) != "" {
			fields = append(fields, zap.String("crypto_original_message", originalMessage))
		}
	}
	if rawProfile, ok := c.Get(cryptoProfileContextKey); ok {
		if profile, ok := rawProfile.(string); ok && strings.TrimSpace(profile) != "" {
			fields = append(fields, zap.String("crypto_profile", profile))
		}
	}
	if reason := strings.TrimSpace(errorReason); reason != "" {
		fields = append(fields, zap.String("error_reason", reason))
	}
	return fields, len(fields) > 0
}

func logCryptoPrefetchResponse(
	reqLog *zap.Logger,
	account *service.Account,
	prepared *service.OpenAICryptoChatPreparation,
) {
	if reqLog == nil || prepared == nil {
		return
	}

	fields := make([]zap.Field, 0, 4)
	if account != nil {
		fields = append(fields, zap.Int64("account_id", account.ID))
	}
	if prepared.PrefetchResult != nil {
		if requestID := strings.TrimSpace(prepared.PrefetchResult.RequestID); requestID != "" {
			fields = append(fields, zap.String("upstream_request_id", requestID))
		}
	}
	if len(prepared.AdapterNames) > 0 {
		fields = append(fields, zap.Strings("crypto_adapter_names", prepared.AdapterNames))
	}
	if len(prepared.ToolCalls) > 0 {
		fields = append(fields, zap.Strings("tool_calls", prepared.ToolCalls))
	}

	reqLog.Info("openai_chat_completions.crypto_provider_response_prepared", fields...)
}

func extractCryptoProfileMessageTextFromParsedRequest(parsed *service.ParsedRequest) string {
	if parsed == nil {
		return ""
	}
	return flattenCryptoProfileMessageTexts(parsed.Messages)
}

func extractCryptoProfileMessageTextFromMessagesBody(body []byte) string {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return ""
	}

	var decoded []any
	if err := json.Unmarshal([]byte(messages.Raw), &decoded); err != nil {
		return ""
	}
	return flattenCryptoProfileMessageTexts(decoded)
}

func flattenCryptoProfileMessageTexts(messages []any) string {
	if len(messages) == 0 {
		return ""
	}

	parts := make([]string, 0, len(messages))
	for _, rawMessage := range messages {
		messageMap, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		role, _ := messageMap["role"].(string)
		if strings.TrimSpace(role) != "" && !strings.EqualFold(role, "user") {
			continue
		}

		switch content := messageMap["content"].(type) {
		case string:
			content = strings.TrimSpace(content)
			if content != "" {
				parts = append(parts, content)
			}
		case []any:
			for _, rawPart := range content {
				partMap, ok := rawPart.(map[string]any)
				if !ok {
					continue
				}
				text, _ := partMap["text"].(string)
				text = strings.TrimSpace(text)
				if text != "" {
					parts = append(parts, text)
				}
			}
		}
	}

	return strings.Join(parts, "\n\n")
}
