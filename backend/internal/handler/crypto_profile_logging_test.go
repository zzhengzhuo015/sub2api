package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type stubCryptoProfileDetector struct {
	enabled bool
	result  *service.CryptoProfileMatchResult
	err     error
	gotText string
}

func (s *stubCryptoProfileDetector) Enabled() bool {
	return s.enabled
}

func (s *stubCryptoProfileDetector) Detect(_ context.Context, message string) (*service.CryptoProfileMatchResult, error) {
	s.gotText = message
	return s.result, s.err
}

func TestMaybeLogCryptoProfileMatch_LogsWhenDetectorMatches(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logSink, restore := captureHandlerStructuredLog(t)
	defer restore()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	detector := &stubCryptoProfileDetector{
		enabled: true,
		result: &service.CryptoProfileMatchResult{
			Matched: true,
			Profile: "dex-routing",
			Model:   "openai/gpt-5.2",
		},
	}

	reqLog := requestLogger(c, "handler.openai_gateway.chat_completions")
	maybeLogCryptoProfileMatch(c.Request.Context(), reqLog, detector, "swap 10 ETH to USDC", "/v1/chat/completions")

	require.Equal(t, "swap 10 ETH to USDC", detector.gotText)
	require.True(t, logSink.ContainsMessageAtLevel("gateway.crypto_profile_matched", "info"))
	require.True(t, logSink.ContainsFieldValue("crypto_profile", "dex-routing"))
	require.True(t, logSink.ContainsFieldValue("entrypoint", "/v1/chat/completions"))
	require.True(t, logSink.ContainsFieldValue("detector_model", "openai/gpt-5.2"))
}

func TestDetectCryptoProfileMatch_ReturnsResultWhenMatched(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logSink, restore := captureHandlerStructuredLog(t)
	defer restore()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	detector := &stubCryptoProfileDetector{
		enabled: true,
		result: &service.CryptoProfileMatchResult{
			Matched: true,
			Profile: "dex-routing",
			Model:   "openai/gpt-5.2",
		},
	}

	reqLog := requestLogger(c, "handler.openai_gateway.chat_completions")
	result := detectCryptoProfileMatch(c.Request.Context(), reqLog, detector, "swap 10 ETH to USDC", "/v1/chat/completions")

	require.NotNil(t, result)
	require.True(t, result.Matched)
	require.Equal(t, "dex-routing", result.Profile)
	require.Equal(t, "swap 10 ETH to USDC", detector.gotText)
	require.True(t, logSink.ContainsMessageAtLevel("gateway.crypto_profile_matched", "info"))
}

func TestDetectCryptoProfileMatch_LogsSkippedWhenDetectorDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logSink, restore := captureHandlerStructuredLog(t)
	defer restore()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	detector := &stubCryptoProfileDetector{enabled: false}
	reqLog := requestLogger(c, "handler.openai_gateway.chat_completions")
	result := detectCryptoProfileMatch(c.Request.Context(), reqLog, detector, "给出btc价格", "/v1/chat/completions")

	require.Nil(t, result)
	require.Empty(t, detector.gotText)
	require.True(t, logSink.ContainsMessageAtLevel("gateway.crypto_profile_detection_skipped", "info"))
	require.True(t, logSink.ContainsFieldValue("reason", "detector_disabled"))
	require.True(t, logSink.ContainsFieldValue("message_chars", "15"))
}

func TestDetectCryptoProfileMatch_LogsNotMatchedResult(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logSink, restore := captureHandlerStructuredLog(t)
	defer restore()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	detector := &stubCryptoProfileDetector{
		enabled: true,
		result: &service.CryptoProfileMatchResult{
			Matched: false,
			Profile: "",
			Model:   "gpt-5.2",
		},
	}

	reqLog := requestLogger(c, "handler.openai_gateway.chat_completions")
	result := detectCryptoProfileMatch(c.Request.Context(), reqLog, detector, "给出btc价格", "/v1/chat/completions")

	require.NotNil(t, result)
	require.False(t, result.Matched)
	require.Equal(t, "给出btc价格", detector.gotText)
	require.True(t, logSink.ContainsMessageAtLevel("gateway.crypto_profile_detection_invoking", "info"))
	require.True(t, logSink.ContainsMessageAtLevel("gateway.crypto_profile_not_matched", "info"))
	require.True(t, logSink.ContainsFieldValue("detector_model", "gpt-5.2"))
}

func TestDetectCryptoProfileMatch_LogsOriginalMessageAndErrorReasonOnDetectorFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logSink, restore := captureHandlerStructuredLog(t)
	defer restore()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	detector := &stubCryptoProfileDetector{
		enabled: true,
		err:     errors.New("upstream detector timeout"),
	}

	reqLog := requestLogger(c, "handler.openai_gateway.chat_completions")
	result := detectCryptoProfileMatch(c.Request.Context(), reqLog, detector, "帮我分析 BTC 和 ETH 走势", "/v1/chat/completions")

	require.Nil(t, result)
	require.True(t, logSink.ContainsMessageAtLevel("gateway.crypto_profile_detection_failed", "warn"))
	require.True(t, logSink.ContainsFieldValue("original_message", "帮我分析 BTC 和 ETH 走势"))
	require.True(t, logSink.ContainsFieldValue("error_reason", "upstream detector timeout"))
}

func TestOpenAIHandleFailoverExhausted_LogsCryptoOriginalMessageAndReason(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logSink, restore := captureHandlerStructuredLog(t)
	defer restore()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	setCryptoProfileLogContext(c, "swap 10 ETH to USDC，分析最佳路径", &service.CryptoProfileMatchResult{
		Matched: true,
		Profile: "dex-routing",
		Model:   "openai/gpt-5.2",
	})

	h := &OpenAIGatewayHandler{}
	h.handleFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode: http.StatusBadGateway,
		ResponseBody: []byte(`{
			"error": {
				"message": "agent upstream overloaded"
			}
		}`),
	}, false)

	require.True(t, logSink.ContainsMessageAtLevel("openai.crypto_upstream_failed", "warn"))
	require.True(t, logSink.ContainsFieldValue("crypto_original_message", "swap 10 ETH to USDC"))
	require.True(t, logSink.ContainsFieldValue("error_reason", "agent upstream overloaded"))
	require.True(t, logSink.ContainsFieldValue("crypto_profile", "dex-routing"))
}

func TestLogCryptoPrefetchResponse_LogsAdapterNamesWhenPresent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logSink, restore := captureHandlerStructuredLog(t)
	defer restore()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	reqLog := requestLogger(c, "handler.openai_gateway.chat_completions")
	logCryptoPrefetchResponse(reqLog, &service.Account{ID: 42}, &service.OpenAICryptoChatPreparation{
		AdapterNames: []string{"dexscreener", "coinglass"},
		ToolCalls:    []string{"crypto-market.fetch_price"},
		PrefetchResult: &service.OpenAIForwardResult{
			RequestID: "rid_crypto_prefetch",
		},
	})

	require.True(t, logSink.ContainsMessageAtLevel("openai_chat_completions.crypto_provider_response_prepared", "info"))
	require.True(t, logSink.ContainsFieldValue("account_id", "42"))
	require.True(t, logSink.ContainsFieldValue("upstream_request_id", "rid_crypto_prefetch"))
	require.True(t, logSink.ContainsFieldValue("crypto_adapter_names", "dexscreener"))
	require.True(t, logSink.ContainsFieldValue("crypto_adapter_names", "coinglass"))
	require.True(t, logSink.ContainsFieldValue("tool_calls", "crypto-market.fetch_price"))
}
